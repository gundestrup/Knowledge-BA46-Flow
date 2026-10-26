// Minimal Chrome DevTools Protocol driver for the KBM-flow browser e2e suite.
// Uses the system Chrome/Chromium and Node's built-in WebSocket — no browser
// automation dependency is vendored for these tests.

import { spawn } from "node:child_process";
import { existsSync } from "node:fs";
import { mkdtemp, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";

const CHROME_CANDIDATES = [
  process.env.KBM_E2E_CHROME,
  process.env.CHROME_PATH,
  "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
  "/Applications/Chromium.app/Contents/MacOS/Chromium",
  "google-chrome",
  "google-chrome-stable",
  "chromium",
  "chromium-browser",
].filter(Boolean);

export function findChrome() {
  for (const candidate of CHROME_CANDIDATES) {
    if (candidate.includes("/") ? existsSync(candidate) : which(candidate)) {
      return candidate;
    }
  }
  return null;
}

function which(name) {
  const path = process.env.PATH || "";
  for (const dir of path.split(":")) {
    const full = join(dir, name);
    if (existsSync(full)) return true;
  }
  return false;
}

async function fetchJSON(url, init) {
  const res = await fetch(url, init);
  if (!res.ok) throw new Error(`${url} -> HTTP ${res.status}`);
  return res.json();
}

export async function waitHTTP(url, timeoutMs = 15000) {
  const deadline = Date.now() + timeoutMs;
  let lastError;
  while (Date.now() < deadline) {
    try {
      return await fetchJSON(url);
    } catch (error) {
      lastError = error;
      await new Promise((resolve) => setTimeout(resolve, 150));
    }
  }
  throw new Error(
    `timeout waiting for ${url}: ${lastError?.message || lastError}`,
  );
}

// CDPClient is a thin JSON-RPC wrapper over one DevTools WebSocket session.
class CDPClient {
  constructor(ws) {
    this.ws = ws;
    this.nextID = 1;
    this.pending = new Map();
    this.handlers = new Map();
    ws.addEventListener("message", (event) => this.#onMessage(event));
  }

  static async connect(url) {
    const ws = new WebSocket(url);
    await new Promise((resolve, reject) => {
      ws.addEventListener("open", resolve, { once: true });
      ws.addEventListener("error", reject, { once: true });
    });
    return new CDPClient(ws);
  }

  #onMessage(event) {
    const message = JSON.parse(event.data);
    if (message.id && this.pending.has(message.id)) {
      const { resolve, reject } = this.pending.get(message.id);
      this.pending.delete(message.id);
      if (message.error) reject(new Error(message.error.message));
      else resolve(message.result);
      return;
    }
    const listeners = this.handlers.get(message.method);
    if (listeners) for (const fn of listeners) fn(message.params || {});
  }

  on(method, fn) {
    if (!this.handlers.has(method)) this.handlers.set(method, []);
    this.handlers.get(method).push(fn);
  }

  send(method, params = {}) {
    const id = this.nextID++;
    return new Promise((resolve, reject) => {
      this.pending.set(id, { resolve, reject });
      this.ws.send(JSON.stringify({ id, method, params }));
    });
  }

  close() {
    try {
      this.ws.close();
    } catch {
      /* already closed */
    }
  }
}

// Page wraps a single Chrome tab target. It records page errors (uncaught
// exceptions and console.error calls) and the network requests the page
// issues, so scenarios can assert both UI state and API traffic.
export class Page {
  constructor(client) {
    this.client = client;
    this.errors = [];
    this.requests = [];
    client.on("Runtime.exceptionThrown", (params) => {
      const detail = params.exceptionDetails || {};
      const url = detail.url || "";
      if (
        url.startsWith("chrome-extension://") ||
        url.startsWith("devtools://")
      )
        return;
      this.errors.push(
        `exception: ${detail.text || ""} ${detail.exception?.description || ""}`.trim(),
      );
    });
    client.on("Runtime.consoleAPICalled", (params) => {
      if (params.type !== "error" && params.type !== "assert") return;
      const first = (params.args || [])
        .map((arg) => arg.value || arg.description || "")
        .join(" ");
      const url = (params.stackTrace?.callFrames || [])[0]?.url || "";
      if (
        url.startsWith("chrome-extension://") ||
        url.startsWith("devtools://")
      )
        return;
      this.errors.push(`console.${params.type}: ${first} (${url})`);
    });
    client.on("Log.entryAdded", (params) => {
      const entry = params.entry || {};
      if (entry.level !== "error") return;
      const url = entry.url || "";
      if (url.startsWith("chrome-extension://")) return;
      if (url.endsWith("/favicon.ico")) return;
      this.errors.push(`log[${entry.source}]: ${entry.text} (${url})`);
    });
    client.on("Network.requestWillBeSent", (params) => {
      const request = params.request || {};
      this.requests.push({
        id: params.requestId,
        method: request.method,
        url: request.url,
        body: request.postData || "",
        status: null,
      });
    });
    client.on("Network.responseReceived", (params) => {
      const entry = this.requests.findLast((e) => e.id === params.requestId);
      if (entry) entry.status = params.response?.status ?? null;
    });
  }

  async eval(expression) {
    const result = await this.client.send("Runtime.evaluate", {
      expression,
      awaitPromise: true,
      returnByValue: true,
    });
    if (result.exceptionDetails) {
      const detail = result.exceptionDetails;
      const description = detail.exception?.description || detail.text;
      throw new Error(
        `page eval failed: ${description} — in: ${expression.slice(0, 120)}`,
      );
    }
    return result.result.value;
  }

  // waitFor polls an expression until it returns truthy; use it to await
  // Alpine-driven DOM updates rather than fixed sleeps.
  async waitFor(
    expression,
    timeoutMs = 10000,
    label = expression.slice(0, 80),
  ) {
    const deadline = Date.now() + timeoutMs;
    while (Date.now() < deadline) {
      if (await this.eval(expression)) return;
      await new Promise((resolve) => setTimeout(resolve, 100));
    }
    throw new Error(`timeout waiting for: ${label}`);
  }

  async click(selector) {
    await this.eval(
      `document.querySelector(${JSON.stringify(selector)}).click()`,
    );
  }

  // clickUntilRequest clicks the button whose text includes `label` until the
  // expected request appears in the network log — absorbing the frame where
  // the button is still :disabled after the previous async job ended.
  async clickUntilRequest(
    label,
    method,
    path,
    { attempts = 4, gapMs = 1200, body } = {},
  ) {
    for (let i = 0; i < attempts; i++) {
      const enabled = await this.eval(`(() => {
        const b = [...document.querySelectorAll("button")].find(
          (x) => x.textContent.includes(${JSON.stringify(label)}));
        if (!b || b.disabled) return false;
        b.click();
        return true;
      })()`);
      if (!enabled) {
        await new Promise((resolve) => setTimeout(resolve, gapMs));
        continue;
      }
      const deadline = Date.now() + 8000;
      while (Date.now() < deadline) {
        if (this.requestSucceeded(method, path, body)) return;
        await new Promise((resolve) => setTimeout(resolve, 100));
      }
    }
    throw new Error(`clicking "${label}" never produced ${method} ${path}`);
  }

  async selectValue(selector, value) {
    await this.eval(`(() => {
      const el = document.querySelector(${JSON.stringify(selector)});
      el.value = ${JSON.stringify(value)};
      el.dispatchEvent(new Event("input", { bubbles: true }));
      el.dispatchEvent(new Event("change", { bubbles: true }));
    })()`);
  }

  requestsMatching(pattern) {
    return this.requests.filter((entry) => entry.url.includes(pattern));
  }

  requestMade(method, path) {
    return this.requests.some(
      (entry) => entry.method === method && entry.url.includes(path),
    );
  }

  // requestSucceeded reports whether a matching request got a <400 response —
  // a 409 op-lock rejection must not satisfy clickUntilRequest's wait.
  requestSucceeded(method, path, bodyIncludes) {
    return this.requests.some(
      (entry) =>
        entry.method === method &&
        entry.url.includes(path) &&
        entry.status !== null &&
        entry.status < 400 &&
        (bodyIncludes === undefined || entry.body.includes(bodyIncludes)),
    );
  }

  requestBody(path) {
    return this.requests.find((entry) => entry.url.includes(path))?.body || "";
  }

  requestSummary() {
    return this.requests
      .map((entry) => `${entry.method} ${entry.url}`)
      .join(", ");
  }
}

// launchBrowser starts headless Chrome with CDP enabled on an ephemeral
// port and returns a handle with a newPage() factory plus close().
export async function launchBrowser(cdpPort) {
  const chrome = findChrome();
  if (!chrome) return null;
  const profile = await mkdtemp(join(tmpdir(), "kbm-e2e-chrome-"));
  const proc = spawn(
    chrome,
    [
      "--headless=new",
      `--remote-debugging-port=${cdpPort}`,
      `--user-data-dir=${profile}`,
      "--no-first-run",
      "--disable-extensions",
      "--disable-component-update",
      "--mute-audio",
      "about:blank",
    ],
    { stdio: ["ignore", "ignore", "ignore"] },
  );
  proc.unref();
  const base = `http://127.0.0.1:${cdpPort}`;
  try {
    await waitHTTP(`${base}/json/version`, 20000);
  } catch (error) {
    proc.kill("SIGKILL");
    await rm(profile, { recursive: true, force: true });
    throw new Error(
      `Chrome did not expose CDP on :${cdpPort}: ${error.message}`,
    );
  }

  return {
    async newPage() {
      // Chrome 111+ requires PUT for /json/new.
      const target = await fetchJSON(`${base}/json/new?about:blank`, {
        method: "PUT",
      });
      const client = await CDPClient.connect(target.webSocketDebuggerUrl);
      const page = new Page(client);
      await client.send("Runtime.enable");
      await client.send("Page.enable");
      await client.send("Log.enable");
      await client.send("Network.enable");
      return page;
    },
    async close() {
      try {
        proc.kill("SIGKILL");
      } catch {
        /* already gone */
      }
      await rm(profile, { recursive: true, force: true });
    },
  };
}
