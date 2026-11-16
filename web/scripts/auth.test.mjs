import { test } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import vm from "node:vm";
import path from "node:path";
import { fileURLToPath } from "node:url";
import ts from "typescript";

const web = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");
function load(file, globals = {}) {
  const source = fs.readFileSync(path.join(web, "src", file), "utf8");
  const output = ts.transpileModule(source, {
    compilerOptions: { module: ts.ModuleKind.CommonJS, target: ts.ScriptTarget.ES2022 },
  }).outputText;
  const exports = {};
  vm.runInNewContext(output, { exports, process: { env: {} }, ...globals });
  return exports;
}

function browser() {
  const values = new Map();
  const cookies = new Map();
  const writes = [];
  const localStorage = {
    getItem(key) { return values.get(key) ?? null; },
    setItem(key, value) { values.set(key, String(value)); },
    removeItem(key) { values.delete(key); },
  };
  const document = {};
  Object.defineProperty(document, "cookie", {
    get() { return [...cookies].map(([k, v]) => `${k}=${v}`).join("; "); },
    set(value) {
      writes.push(value);
      const [pair, ...attrs] = value.split(";");
      const separator = pair.indexOf("=");
      const key = pair.slice(0, separator);
      const val = pair.slice(separator + 1);
      if (attrs.some((a) => a.trim().toLowerCase() === "max-age=0")) cookies.delete(key);
      else cookies.set(key, val);
    },
  });
  const window = { location: { href: "" } };
  const auth = load("lib/auth.ts", { window, document, localStorage }).auth;
  return { auth, window, document, localStorage, cookies, writes };
}

test("login stores the token only in origin-scoped storage and expires the legacy cookie", () => {
  const b = browser();
  b.cookies.set("artex_token", "old-secret");
  b.auth.setToken("dummy-secret");
  assert.equal(b.localStorage.getItem("artex_token"), "dummy-secret");
  assert.equal(b.cookies.has("artex_token"), false);
  assert.equal(b.cookies.get("artex_session"), "1");
  assert.equal(b.writes.some((write) => write.includes("dummy-secret")), false);
});

test("existing localStorage sessions migrate away from the legacy credential cookie", () => {
  const b = browser();
  b.cookies.set("artex_token", "old-secret");
  b.localStorage.setItem("artex_token", "dummy-secret");
  assert.equal(b.auth.getToken(), "dummy-secret");
  assert.equal(b.cookies.has("artex_token"), false);
  assert.equal(b.cookies.get("artex_session"), "1");
});

test("logout and missing-token refresh clear both legacy cookie and redirect hint", () => {
  const b = browser();
  b.auth.setToken("dummy-secret");
  b.cookies.set("artex_token", "old-secret");
  b.auth.clearToken();
  assert.equal(b.auth.getToken(), null);
  assert.equal(b.localStorage.getItem("artex_token"), null);
  assert.equal(b.cookies.has("artex_token"), false);
  assert.equal(b.cookies.has("artex_session"), false);
});

test("development proxy treats the non-secret hint as a redirect hint only", () => {
  const next = { next: () => ({ type: "next" }), redirect: (url) => ({ type: "redirect", url: url.pathname }) };
  const proxy = load("proxy.ts", { URL, require: (id) => {
    if (id === "next/server") return { NextResponse: next };
    throw new Error(`unexpected import ${id}`);
  } }).proxy;
  function request(pathname, cookies) {
    return { url: `http://localhost:5173${pathname}`, nextUrl: { pathname }, cookies: { get: (name) => cookies[name] ? { value: cookies[name] } : undefined } };
  }
  assert.equal(proxy(request("/function/tasks", {})).url, "/login");
  assert.equal(proxy(request("/function/tasks", { artex_token: "old-secret" })).url, "/login");
  assert.equal(proxy(request("/function/tasks", { artex_session: "1" })).type, "next");
  assert.equal(proxy(request("/login", { artex_session: "1" })).url, "/function/tasks");
  assert.equal(proxy(request("/login", {})).type, "next");
});

test("an unauthorized API response clears stored token and both cookies", async () => {
  const b = browser();
  b.auth.setToken("dummy-secret");
  b.cookies.set("artex_token", "old-secret");
  const api = load("lib/api.ts", {
    window: b.window, localStorage: b.localStorage, document: b.document,
    fetch: async () => ({ status: 401 }),
    require: (id) => {
      if (id === "@/lib/auth") return { auth: b.auth };
      if (id === "@/lib/mock/enabled") return { MOCK: false };
      if (id === "@/lib/mock/handler") return { mockHandle: () => {} };
      throw new Error(`unexpected import ${id}`);
    },
  });
  await assert.rejects(api.http("/test"), /Unauthorized/);
  assert.equal(b.localStorage.getItem("artex_token"), null);
  assert.equal(b.cookies.has("artex_token"), false);
  assert.equal(b.cookies.has("artex_session"), false);
  assert.equal(b.window.location.href, "/login");
});
