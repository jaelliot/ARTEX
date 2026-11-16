const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const vm = require('node:vm');

function extension() {
  const callbacks = {};
  const posts = [];
  let rejectUploads = false;
  const storage = { serverUrl: 'http://127.0.0.1:8787', token: 'dummy' };
  const chrome = {
    storage: {
      local: { get: async () => storage, set: async () => {} },
      session: { get: async () => ({}), set: async () => {} },
    },
    tabs: { query: async () => [{ id: 7 }] },
    debugger: {
      attach: async () => {}, detach: async () => {},
      sendCommand: async () => ({ body: 'response', base64Encoded: false }),
      onEvent: { addListener: (fn) => { callbacks.event = fn; } },
      onDetach: { addListener: (fn) => { callbacks.detach = fn; } },
    },
    action: { setBadgeText: async () => {}, setBadgeBackgroundColor: async () => {} },
    runtime: { onMessage: { addListener: (fn) => { callbacks.message = fn; } } },
  };
  const fetch = async (_url, options) => {
    posts.push(JSON.parse(options.body));
    if (rejectUploads) throw new Error('offline');
    return { ok: true, json: async () => ({ ingested: posts.at(-1).exchanges.length, failed: 0 }) };
  };
  const context = vm.createContext({ chrome, fetch, console, setTimeout, clearTimeout });
  vm.runInContext(fs.readFileSync('dist/background.js', 'utf8'), context);
  return { context, callbacks, posts, storage, reject: (value) => { rejectUploads = value; } };
}

test('truncates request body when response is binary or unavailable', async () => {
  const x = extension();
  vm.runInContext('session.capturing = true; session.tabId = 7', x.context);
  const request = { requestId: '1', method: 'POST', url: 'https://example.invalid/', headers: {}, postData: 'a'.repeat(262244) };
  await x.context.queueExchange(request, { headers: { 'content-type': 'image/png' }, status: 200 });
  const captured = vm.runInContext('session.queue[0].requestBody', x.context);
  assert.equal(captured, 'a'.repeat(262144) + '\n... [truncated by extension]');
});

test('removes failed requests from pending map', () => {
  const x = extension();
  vm.runInContext('session.capturing = true; session.tabId = 7', x.context);
  x.callbacks.event({ tabId: 7 }, 'Network.requestWillBeSent', { requestId: 'broken', request: { method: 'GET', url: 'https://example.invalid/', headers: {} } });
  assert.equal(vm.runInContext('session.requests.size', x.context), 1);
  x.callbacks.event({ tabId: 7 }, 'Network.loadingFailed', { requestId: 'broken' });
  assert.equal(vm.runInContext('session.requests.size', x.context), 0);
});

test('stop retains unsent traffic, retries it before starting, and drains every batch', async () => {
  const x = extension();
  vm.runInContext('session.capturing = true; session.tabId = 7; session.queue = Array.from({length: 25}, (_, i) => ({method:"GET",url:"https://example.invalid/" + i,status:200,requestHeaders:{},requestBody:"",responseHeaders:{},responseBody:""}))', x.context);
  x.reject(true);
  const stopped = await x.context.stop();
  assert.equal(stopped.ok, false);
  assert.equal(vm.runInContext('session.queue.length', x.context), 25);
  const blocked = await x.context.start();
  assert.equal(blocked.ok, false);
  assert.equal(vm.runInContext('session.queue.length', x.context), 25);
  x.reject(false);
  const started = await x.context.start();
  assert.equal(started.ok, true);
  assert.equal(vm.runInContext('session.queue.length', x.context), 0);
  assert.deepEqual(x.posts.slice(-3).map((b) => b.exchanges.length), [10, 10, 5]);
});

test('reports newer entries dropped when an in-flight batch fails into a full queue', async () => {
  const x = extension();
  let begin;
  let release;
  const started = new Promise((resolve) => { begin = resolve; });
  const blocked = new Promise((resolve) => { release = resolve; });
  x.context.fetch = async () => { begin(); await blocked; throw new Error('offline'); };
  vm.runInContext('session.queue = Array.from({length: 10}, (_, i) => ({method:"GET",url:String(i),status:200,requestHeaders:{},requestBody:"",responseHeaders:{},responseBody:""}))', x.context);
  const send = x.context.flush();
  await started;
  vm.runInContext('session.queue = Array.from({length: 200}, (_, i) => ({method:"GET",url:String(i + 10),status:200,requestHeaders:{},requestBody:"",responseHeaders:{},responseBody:""}))', x.context);
  release();
  await send;
  assert.equal(vm.runInContext('session.queue.length', x.context), 200);
  assert.equal(vm.runInContext('session.failed', x.context), 10);
  assert.match(vm.runInContext('session.lastError', x.context), /10 newer entries dropped/);
});

test('rejects start while stop is still detaching', async () => {
  const x = extension();
  let began;
  let release;
  const started = new Promise((resolve) => { began = resolve; });
  const pending = new Promise((resolve) => { release = resolve; });
  x.context.chrome.debugger.sendCommand = async (_target, command) => {
    if (command === 'Network.disable') { began(); await pending; }
    return {};
  };
  vm.runInContext('session.capturing = true; session.tabId = 7', x.context);
  const stopping = x.context.stop();
  await started;
  const start = await x.context.start();
  assert.equal(start.ok, false);
  release();
  assert.equal((await stopping).ok, true);
  assert.equal(vm.runInContext('session.capturing', x.context), false);
  assert.equal(vm.runInContext('session.tabId', x.context), null);
});

test('does not send retained traffic to a different configured server', async () => {
  const x = extension();
  assert.equal((await x.context.start()).ok, true);
  vm.runInContext('session.queue.push({method:"GET",url:"https://example.invalid/",status:200,requestHeaders:{},requestBody:"",responseHeaders:{},responseBody:""})', x.context);
  x.storage.serverUrl = 'https://other.example.invalid';
  assert.equal((await x.context.stop()).ok, false);
  assert.equal(x.posts.length, 0);
  assert.equal(vm.runInContext('session.queue.length', x.context), 1);
  assert.equal((await x.context.start()).ok, false);
  assert.equal(x.posts.length, 0);
  x.storage.serverUrl = 'http://127.0.0.1:8787';
  await x.context.flush();
  assert.equal(x.posts.length, 1);
  assert.equal(vm.runInContext('session.queue.length', x.context), 0);
});

test('does not rebind an in-flight failed batch after debugger detach', async () => {
  const x = extension();
  assert.equal((await x.context.start()).ok, true);
  let began;
  let release;
  const started = new Promise((resolve) => { began = resolve; });
  const pending = new Promise((resolve) => { release = resolve; });
  let calls = 0;
  x.context.fetch = async () => { calls++; began(); await pending; throw new Error('offline'); };
  vm.runInContext('session.queue.push({method:"GET",url:"https://example.invalid/",status:200,requestHeaders:{},requestBody:"",responseHeaders:{},responseBody:""})', x.context);
  const sending = x.context.flush();
  await started;
  x.callbacks.detach({ tabId: 7 });
  x.storage.serverUrl = 'https://other.example.invalid';
  const restarting = x.context.start();
  release();
  await sending;
  assert.equal((await restarting).ok, false);
  assert.equal(calls, 1);
  assert.equal(vm.runInContext('session.queue.length', x.context), 1);
});

test('capture upload refuses redirects', async () => {
  const x = extension();
  let options;
  x.context.fetch = async (_url, request) => {
    options = request;
    return { ok: true, json: async () => ({ ingested: 1, failed: 0 }) };
  };
  vm.runInContext('session.queue.push({method:"GET",url:"https://example.invalid/",status:200,requestHeaders:{},requestBody:"",responseHeaders:{},responseBody:""})', x.context);
  await x.context.flush();
  assert.equal(options.redirect, 'error');
});

test('popup preserves action errors during periodic refresh', async () => {
  const elements = {};
  for (const id of ['status', 'error', 'serverUrl', 'token', 'save', 'start', 'stop', 'flush']) {
    elements[id] = { value: '', textContent: '', addEventListener(_name, callback) { this.click = callback; } };
  }
  let interval;
  const chrome = {
    storage: { local: { get: async () => ({}), set: async () => {} } },
    runtime: { sendMessage: async (message) => message.type === 'start'
      ? { ok: false, error: 'pending traffic' }
      : { capturing: false, tabId: null, ingested: 0, queued: 1, failed: 0, lastError: '' } },
  };
  const context = vm.createContext({ chrome, document: { getElementById: (id) => elements[id] },
    setInterval: (callback) => { interval = callback; }, setTimeout });
  vm.runInContext(fs.readFileSync('dist/popup.js', 'utf8'), context);
  await new Promise((resolve) => setImmediate(resolve));
  await elements.start.click();
  assert.equal(elements.error.textContent, 'pending traffic');
  await interval();
  assert.equal(elements.error.textContent, 'pending traffic');
});

test('detaches and reports an error when enabling network capture fails', async () => {
  const x = extension();
  let detached = 0;
  x.context.chrome.debugger.sendCommand = async () => { throw new Error('not available'); };
  x.context.chrome.debugger.detach = async () => { detached++; };
  const result = await x.context.start();
  assert.equal(result.ok, false);
  assert.match(result.error, /Network.enable failed/);
  assert.equal(detached, 1);
  assert.equal(vm.runInContext('session.capturing', x.context), false);
});
