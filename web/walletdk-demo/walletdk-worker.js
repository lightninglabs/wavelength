let wasmReady = false;
let loadPromise = null;

function postEvent(type, payload) {
  self.postMessage({
    event: {
      type,
      payload,
    },
  });
}

function rejectAllPending(error) {
  postEvent("log", { level: "error", message: String(error?.message || error) });
}

self.addEventListener("walletdk-ready", () => {
  wasmReady = true;
  postEvent("runtimeReady");
});

self.onmessage = async (event) => {
  const { id, method, params } = event.data || {};

  try {
    await ensureLoaded();

    if (method === "$ready") {
      self.postMessage({ id, ok: true, result: { ready: true } });

      return;
    }

    const result = await self.walletdkCall(method, params || {});
    publishSQLiteOpenResults();
    self.postMessage({ id, ok: true, result });
  } catch (err) {
    publishSQLiteOpenResults();
    self.postMessage({
      id,
      ok: false,
      error: String(err?.message || err),
    });
  }
};

async function ensureLoaded() {
  if (wasmReady) {
    return;
  }

  if (!loadPromise) {
    loadPromise = loadRuntime();
  }

  await loadPromise;
}

async function loadRuntime() {
  if (typeof self.CustomEvent !== "function") {
    self.CustomEvent = class CustomEvent extends Event {
      constructor(type, params = {}) {
        super(type, params);
        this.detail = params.detail;
      }
    };
  }

  importScripts("sqlite-bridge.js");
  importScripts("wasm_exec.js");

  const go = new Go();
  const result = await instantiateWasm(go.importObject);
  const runPromise = go.run(result.instance);
  runPromise.catch(rejectAllPending);

  await waitForWASMReady();
}

function waitForWASMReady() {
  if (wasmReady) {
    return Promise.resolve();
  }

  return new Promise((resolve) => {
    self.addEventListener("walletdk-ready", () => resolve(), { once: true });
  });
}

async function instantiateWasm(importObject) {
  if ("DecompressionStream" in self) {
    try {
      return await instantiateCompressedWasm(importObject);
    } catch (err) {
      postEvent("log", {
        level: "warn",
        message: `compressed wasm load failed: ${String(err?.message || err)}`,
      });
    }
  }

  return instantiateRawWasm(importObject);
}

async function instantiateCompressedWasm(importObject) {
  const response = await fetch("walletdk.wasm.gz");
  if (!response.ok) {
    throw new Error("walletdk compressed wasm artifact not found");
  }

  const stream = response.body.pipeThrough(new DecompressionStream("gzip"));
  const bytes = await new Response(stream).arrayBuffer();

  return WebAssembly.instantiate(bytes, importObject);
}

async function instantiateRawWasm(importObject) {
  const response = await fetch("walletdk.wasm");
  if (!response.ok) {
    throw new Error("walletdk wasm artifact not found");
  }

  return WebAssembly.instantiateStreaming(response, importObject);
}

function publishSQLiteOpenResults() {
  if (!self.sqliteBridgeOpenResults) {
    return;
  }

  postEvent("sqliteOpenResults", self.sqliteBridgeOpenResults);
}
