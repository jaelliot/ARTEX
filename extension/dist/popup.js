"use strict";
function element(id) {
    const result = document.getElementById(id);
    if (!result)
        throw new Error(`Missing element: ${id}`);
    return result;
}
const statusElement = element("status");
const errorElement = element("error");
const serverUrlInput = element("serverUrl");
const tokenInput = element("token");
const saveButton = element("save");
let actionError = "";
async function refreshState() {
    const st = await chrome.runtime.sendMessage({ type: "getState" });
    const lines = [];
    lines.push(st.capturing
        ? `● Recording tab ${st.tabId}`
        : "○ Not recording");
    lines.push(`ingested: ${st.ingested} · queued: ${st.queued} · failed: ${st.failed}`);
    if (st.lastError)
        lines.push(st.lastError);
    statusElement.textContent = lines.join("\n");
    errorElement.textContent = actionError || (st.capturing ? "" : (st.lastError || ""));
}
saveButton.addEventListener("click", async () => {
    await chrome.storage.local.set({
        serverUrl: serverUrlInput.value.trim(),
        token: tokenInput.value.trim(),
    });
    saveButton.textContent = "Saved ✓";
    setTimeout(() => (saveButton.textContent = "Save settings"), 1200);
});
element("start").addEventListener("click", async () => {
    const out = await chrome.runtime.sendMessage({ type: "start" });
    actionError = out && out.ok === false ? out.error || "failed to start" : "";
    await refreshState();
});
element("stop").addEventListener("click", async () => {
    const out = await chrome.runtime.sendMessage({ type: "stop" });
    actionError = out && out.ok === false ? out.error || "failed to stop" : "";
    await refreshState();
});
element("flush").addEventListener("click", async () => {
    await chrome.runtime.sendMessage({ type: "flushNow" });
    actionError = "";
    refreshState();
});
(async () => {
    const { serverUrl = "", token = "" } = await chrome.storage.local.get(["serverUrl", "token"]);
    serverUrlInput.value = typeof serverUrl === "string" ? serverUrl : "";
    tokenInput.value = typeof token === "string" ? token : "";
    refreshState();
    setInterval(refreshState, 1500);
})();
