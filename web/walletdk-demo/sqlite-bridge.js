// Main-thread bridge between Go WASM and the dedicated SQLite oo1 worker.
// This demo-local copy tracks go-wasmsqlite's bridge and resets the worker
// after failed opens so browser reloads can retry OPFS handles cleanly.

(() => {
  let worker = null;
  let nextRequestId = 1;
  let initPromise = null;
  const pending = new Map();
  const bridgeScriptURL = globalThis.document?.currentScript?.src || "";
  const openResults = globalThis.sqliteBridgeOpenResults || [];
  globalThis.sqliteBridgeOpenResults = openResults;

  function workerURL(override) {
    if (override) return override;
    if (globalThis.sqliteBridgeWorkerURL) return globalThis.sqliteBridgeWorkerURL;
    if (bridgeScriptURL) return new URL("sqlite-worker.js", bridgeScriptURL).href;
    return "sqlite-worker.js";
  }

  function ensureWorker(override) {
    if (worker) return;

    worker = new Worker(workerURL(override));
    worker.onmessage = (event) => {
      const message = event.data || {};
      const waiter = pending.get(message.id);
      if (!waiter) return;

      pending.delete(message.id);
      if (message.ok) {
        waiter.resolve(message.result || {});
      } else {
        waiter.reject(new Error(message.error || "sqlite worker request failed"));
      }
    };
    worker.onerror = (event) => {
      const err = new Error(event.message || "sqlite worker error");
      for (const waiter of pending.values()) waiter.reject(err);
      pending.clear();
    };
  }

  function resetWorker() {
    if (!worker) return;

    for (const waiter of pending.values()) {
      waiter.reject(new Error("SQLite worker reset"));
    }
    pending.clear();
    worker.terminate();
    worker = null;
    initPromise = null;
  }

  function request(method, args = {}, transfer = []) {
    if (!worker) throw new Error("SQLite worker is not initialized");

    const id = nextRequestId++;
    const promise = new Promise((resolve, reject) => {
      pending.set(id, { resolve, reject });
    });
    worker.postMessage({ id, method, args }, transfer);
    return promise;
  }

  globalThis.sqliteBridge = {
    async init(options = {}) {
      ensureWorker(options.workerURL);
      if (!initPromise) {
        initPromise = request("init", { sqliteJSURL: options.sqliteJSURL || globalThis.sqliteBridgeSQLiteJSURL || "" }).then((result) => {
          console.log("SQLite oo1 worker initialized:", result.version?.libVersion || "unknown");
          return { ok: true, ...result };
        });
      }
      return initPromise;
    },

    async open(optionsOrFilename = {}, maybeVFS = "opfs") {
      await globalThis.sqliteBridge.init();

      let options = optionsOrFilename;
      if (typeof optionsOrFilename === "string") {
        options = { file: optionsOrFilename, vfs: maybeVFS };
      }

      let result;
      try {
        result = await request("open", options || {});
      } catch (error) {
        resetWorker();
        throw error;
      }

      const opened = {
        ok: true,
        dbId: result.dbId,
        filename: result.filename,
        vfsType: result.vfs === "memory" ? "memory" : (result.persistent ? "opfs" : result.vfs),
        resolvedVFS: result.vfs,
        persistent: !!result.persistent
      };
      openResults.push(opened);

      return opened;
    },

    async exec(dbId, sql, params = []) {
      const result = await request("exec", { dbId, sql, params });
      return { ok: true, ...result };
    },

    async query(dbId, sql, params = []) {
      const result = await request("query", { dbId, sql, params });
      return {
        ok: true,
        columns: result.columnNames || [],
        rows: result.resultRows || []
      };
    },

    async begin(dbId) {
      await request("begin", { dbId });
      return { ok: true };
    },

    async commit(dbId) {
      await request("commit", { dbId });
      return { ok: true };
    },

    async rollback(dbId) {
      await request("rollback", { dbId });
      return { ok: true };
    },

    async close(dbId) {
      await request("close", { dbId });
      return { ok: true };
    },

    async dump(dbId) {
      const result = await request("dump", { dbId });
      return { ok: true, dump: result.dump || "" };
    },

    async load(dbId, sql) {
      await request("load", { dbId, sql });
      return { ok: true };
    }
  };
})();
