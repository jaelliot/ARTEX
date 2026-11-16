// ARTEX Traffic Capture — background service worker.
//
// Captures the network activity of one tab through the chrome.debugger API
// (Network domain) and pushes text request/response pairs to the ARTEX
// ingest endpoint in batches. State lives in chrome.storage.session so the
// popup and the (short-lived) service worker share one view; the queue is
// deliberately in-memory — a browser restart drops unsent entries, which is
// the acceptable failure mode for a manual capture session.

const FLUSH_AT = 10; // entries per batch
const MAX_QUEUE = 200;
const MAX_BODY = 256 * 1024; // matches the server-side ingest cap
const SKIP_MIME = ["image/", "font/", "audio/", "video/", "application/octet-stream"];

interface CapturedRequest {
  requestId: string;
  method: string;
  url: string;
  headers: Record<string, string>;
  postData: string;
  response?: CapturedResponse;
}
interface CapturedResponse {
  status?: number;
  headers?: Record<string, string> | Array<{ name: string; value: string }>;
}
interface Exchange {
  method: string;
  url: string;
  status: number;
  requestHeaders: Record<string, string>;
  requestBody: string;
  responseHeaders: Record<string, string>;
  responseBody: string;
}
interface CaptureSession {
  capturing: boolean;
  tabId: number | null;
  queue: Exchange[];
  requests: Map<string, CapturedRequest>;
  ingested: number;
  failed: number;
  lastError: string;
}

let session: CaptureSession = {
  capturing: false,
  tabId: null,
  queue: [], // pending ExternalExchange objects
  requests: new Map(), // debugger requestId -> partial request info
  ingested: 0,
  failed: 0,
  lastError: "",
};

async function saveState() {
  await chrome.storage.session.set({
    capturing: session.capturing,
    tabId: session.tabId,
    ingested: session.ingested,
    failed: session.failed,
    lastError: session.lastError,
    queued: session.queue.length,
  });
}

async function getConfig() {
  const { serverUrl = "", token = "" } = await chrome.storage.local.get(["serverUrl", "token"]);
  return {
    serverUrl: typeof serverUrl === "string" ? serverUrl.replace(/\/+$/, "") : "",
    token: typeof token === "string" ? token : "",
  };
}

async function getActiveTabId() {
  const [tab] = await chrome.tabs.query({ active: true, currentWindow: true });
  return tab ? tab.id : null;
}

const pendingExchanges = new Set<Promise<void>>();
let flushing: Promise<void> | null = null;
let changingCapture = false;
let captureServerUrl = "";

async function start() {
  if (changingCapture) return { ok: false, error: "Capture is changing; try again." };
  changingCapture = true;
  try {
    return await startCapture();
  } finally {
    changingCapture = false;
  }
}

async function startCapture() {
  if (session.capturing) return { ok: true };
  await Promise.allSettled([...pendingExchanges]);
  if (flushing) await flushing;
  if (session.queue.length) {
    await flush();
    if (session.queue.length) return { ok: false, error: "Pending traffic could not be sent; check settings and retry." };
  }
  const { serverUrl, token } = await getConfig();
  if (!serverUrl || !token) {
    return { ok: false, error: "Set the ARTEX URL and token first." };
  }
  const tabId = await getActiveTabId();
  if (tabId == null) return { ok: false, error: "No active tab." };
  try {
    await chrome.debugger.attach({ tabId }, "1.3");
  } catch (e) {
    return { ok: false, error: `attach failed: ${errorMessage(e)}` };
  }
  try {
    await chrome.debugger.sendCommand({ tabId }, "Network.enable", {});
  } catch (e) {
    try {
      await chrome.debugger.detach({ tabId });
    } catch (_) {
      // The debugger may already be detached.
    }
    return { ok: false, error: `Network.enable failed: ${errorMessage(e)}` };
  }
  captureServerUrl = serverUrl;
  session = { ...session, capturing: true, tabId, queue: [], requests: new Map(), ingested: 0, failed: 0, lastError: "" };
  chrome.action.setBadgeText({ text: "REC" });
  chrome.action.setBadgeBackgroundColor({ color: "#c2410c" });
  await saveState();
  return { ok: true };
}

async function stop() {
  if (changingCapture) return { ok: false, error: "Capture is changing; try again." };
  changingCapture = true;
  try {
    return await stopCapture();
  } finally {
    changingCapture = false;
  }
}

async function stopCapture() {
  const tabId = session.tabId;
  const wasCapturing = session.capturing;
  session.capturing = false;
  if (wasCapturing && tabId !== null) {
    try {
      await chrome.debugger.sendCommand({ tabId }, "Network.disable", {});
    } catch (_) {
      // Detach must still be attempted if the tab has stopped reporting events.
    }
    try {
      await chrome.debugger.detach({ tabId });
    } catch (_) {
      // The browser may already have detached the debugger.
    }
  }
  await Promise.allSettled([...pendingExchanges]);
  await flush();
  session.tabId = null;
  session.requests.clear();
  chrome.action.setBadgeText({ text: "" });
  await saveState();
  if (session.queue.length) return { ok: false, error: "Some captured traffic could not be sent; use Flush now to retry." };
  return { ok: true };
}

function errorMessage(error: unknown): string {
  return error instanceof Error ? error.message : String(error);
}

function skipBody(mimeType: string): boolean {
  const mt = (mimeType || "").toLowerCase();
  return SKIP_MIME.some((p) => mt.startsWith(p));
}

function truncate(s: string): string {
  if (s.length <= MAX_BODY) return s;
  return s.slice(0, MAX_BODY) + "\n... [truncated by extension]";
}

async function queueExchange(info: CapturedRequest, response: CapturedResponse): Promise<void> {
  const respHeaders: Record<string, string> = {};
  // CDP hands back response.headers as a plain object (dict), not an array of
  // {name,value} records; iterating it with for...of throws "object is not
  // iterable" and no exchange is ever queued. Accept both shapes.
  const rawHeaders = response.headers || {};
  if (Array.isArray(rawHeaders)) {
    for (const h of rawHeaders) {
      if (h && h.name != null) respHeaders[String(h.name).toLowerCase()] = h.value;
    }
  } else {
    for (const [k, v] of Object.entries(rawHeaders)) respHeaders[String(k).toLowerCase()] = v;
  }
  const mime = respHeaders["content-type"] || "";

  const reqBody = truncate(info.postData || "");
  let respBody = "";
  if (session.tabId !== null && !skipBody(mime)) {
    try {
      const body = await chrome.debugger.sendCommand(
        { tabId: session.tabId },
        "Network.getResponseBody",
        { requestId: info.requestId }
      );
      const captured = body as { base64Encoded?: boolean; body?: string } | undefined;
      if (captured && !captured.base64Encoded) {
        respBody = truncate(captured.body || "");
      }
    } catch (_) {
      // body already discarded by the browser; capture the pair without bodies
    }
  }

  if (session.queue.length >= MAX_QUEUE) {
    session.failed++;
    session.lastError = "Capture queue full; an exchange was dropped.";
    await saveState();
    return;
  }
  session.queue.push({
    method: info.method,
    url: info.url,
    status: response.status || 0,
    requestHeaders: info.headers || {},
    requestBody: reqBody,
    responseHeaders: respHeaders,
    responseBody: respBody,
  });
  if (session.queue.length >= FLUSH_AT) await flush();
}

async function flush(): Promise<void> {
  if (flushing) return flushing;
  flushing = (async () => {
    while (session.queue.length > 0) {
      const { serverUrl, token } = await getConfig();
      if (!serverUrl || !token) {
        session.lastError = "missing server URL or token";
        break;
      }
      if (captureServerUrl && serverUrl !== captureServerUrl) {
        session.lastError = "Server URL changed; restore the capture server to send pending traffic.";
        break;
      }
      const batch = session.queue.splice(0, FLUSH_AT);
      try {
        const res = await fetch(`${serverUrl}/api/traffic/ingest`, {
          method: "POST",
          redirect: "error",
          headers: { "Authorization": `Bearer ${token}`, "Content-Type": "application/json" },
          body: JSON.stringify({ exchanges: batch }),
        });
        if (!res.ok) throw new Error(`HTTP ${res.status}: ${(await res.text()).slice(0, 200)}`);
        const out = await res.json() as { ingested?: number; failed?: number };
        session.ingested += out.ingested || 0;
        session.failed += out.failed || 0;
        session.lastError = out.failed ? `${out.failed} entries rejected by the server` : "";
      } catch (e) {
        const retained = batch.concat(session.queue);
        const dropped = Math.max(0, retained.length - MAX_QUEUE);
        session.queue = retained.slice(0, MAX_QUEUE);
        session.failed += dropped;
        session.lastError = errorMessage(e);
        if (dropped) session.lastError += `; ${dropped} newer entries dropped because the queue is full`;
        break;
      }
    }
    await saveState();
  })();
  try {
    await flushing;
  } finally {
    flushing = null;
  }
}

chrome.debugger.onEvent.addListener((source, method, params) => {
  if (!session.capturing || !source.tabId || source.tabId !== session.tabId) return;
  if (!params || typeof params !== "object") return;
  if (method === "Network.requestWillBeSent") {
    const event = params as { requestId: string; request: { method: string; url: string; headers: Record<string, string>; postData?: string } };
    session.requests.set(event.requestId, {
      requestId: event.requestId,
      method: event.request.method,
      url: event.request.url,
      headers: event.request.headers,
      postData: event.request.postData || "",
    });
  } else if (method === "Network.responseReceived") {
    const event = params as { requestId: string; response: CapturedResponse };
    const info = session.requests.get(event.requestId);
    if (info) info.response = event.response;
  } else if (method === "Network.loadingFailed") {
    session.requests.delete((params as { requestId: string }).requestId);
  } else if (method === "Network.loadingFinished") {
    const event = params as { requestId: string };
    const info = session.requests.get(event.requestId);
    session.requests.delete(event.requestId);
    if (!info || !info.response) return;
    const pending = queueExchange(info, info.response).catch((e: unknown) => {
      session.lastError = errorMessage(e);
      return saveState();
    });
    pendingExchanges.add(pending);
    void pending.finally(() => pendingExchanges.delete(pending));
  }
});

chrome.debugger.onDetach.addListener((source) => {
  if (session.capturing && source.tabId === session.tabId) {
    // DevTools opened, or the tab navigated somewhere undebaggable: stop
    // cleanly instead of silently dropping events.
    session.capturing = false;
    session.tabId = null;
    session.lastError = "debugger detached (DevTools opened?)";
    chrome.action.setBadgeText({ text: "" });
    saveState();
  }
});

chrome.runtime.onMessage.addListener((msg, _sender, sendResponse) => {
  (async () => {
    const message = msg as { type?: string } | null;
    switch (message?.type) {
      case "getState": {
        sendResponse({
          capturing: session.capturing,
          tabId: session.tabId,
          ingested: session.ingested,
          failed: session.failed,
          queued: session.queue.length,
          lastError: session.lastError,
          configured: !!(await getConfig()).serverUrl,
        });
        return;
      }
      case "start":
        sendResponse(await start());
        return;
      case "stop":
        sendResponse(await stop());
        return;
      case "flushNow":
        await flush();
        sendResponse(await stateSnapshot());
        return;
      default:
        sendResponse({ ok: false, error: "unknown message" });
    }
  })();
  return true; // async sendResponse
});

async function stateSnapshot() {
  return {
    capturing: session.capturing,
    tabId: session.tabId,
    ingested: session.ingested,
    failed: session.failed,
    queued: session.queue.length,
    lastError: session.lastError,
  };
}
