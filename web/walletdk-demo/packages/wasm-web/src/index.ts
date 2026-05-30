import {
  Balance,
  CreateWalletRequest,
  CreateWalletResult,
  DepositRequest,
  DepositResult,
  ExitRequest,
  ExitResult,
  ExitStatusRequest,
  ExitStatusResult,
  ListRequest,
  ListResult,
  ReceiveRequest,
  ReceiveResult,
  RuntimeConfig,
  SendRequest,
  SendResult,
  UnlockWalletRequest,
  UnlockWalletResult,
  WalletDKClient,
  WalletDKError,
  WalletDKEvent,
  WalletDKListener,
  WalletInfo,
  WalletStatus,
} from "@lightninglabs/walletdk-core";

type PendingCall = {
  resolve: (value: unknown) => void;
  reject: (err: Error) => void;
};

export type WasmWalletDKClientOptions = {
  workerURL?: string;
};

export function createWasmWalletDKClient(
  options: WasmWalletDKClientOptions = {},
): WalletDKClient {
  return new MainThreadWalletDKClient(options);
}

export class MainThreadWalletDKClient implements WalletDKClient {
  private readonly listeners = new Set<WalletDKListener>();
  private loadPromise: Promise<void> | null = null;

  constructor(_options: WasmWalletDKClientOptions = {}) {
    globalThis.addEventListener("walletdk-ready", () => {
      this.emit({ type: "runtimeReady" });
    });
    globalThis.addEventListener("walletdk-activity", (event) => {
      const detail = (event as CustomEvent).detail;
      this.emit({ type: "activity", payload: detail });
    });
  }

  ready(): Promise<void> {
    return this.ensureLoaded();
  }

  start(config: RuntimeConfig): Promise<WalletInfo> {
    return this.callRaw<WalletInfo>("start", config);
  }

  async stop(): Promise<void> {
    await this.callRaw("stop");
    this.emit({ type: "runtimeStopped" });
  }

  getInfo(): Promise<WalletInfo> {
    return this.callRaw<WalletInfo>("getInfo");
  }

  status(): Promise<WalletStatus> {
    return this.callRaw<WalletStatus>("status");
  }

  balance(): Promise<Balance> {
    // getRawBalance returns the daemon's per-category breakdown (VTXO,
    // boarding confirmed/unconfirmed, on-chain). The flat "balance" verb
    // collapses everything into a single confirmed figure, which cannot
    // represent a pending boarding deposit or an honest composition.
    return this.callRaw<Balance>("getRawBalance");
  }

  createWallet(req: CreateWalletRequest): Promise<CreateWalletResult> {
    return this.callRaw<CreateWalletResult>("createWallet", req);
  }

  unlockWallet(req: UnlockWalletRequest): Promise<UnlockWalletResult> {
    return this.callRaw<UnlockWalletResult>("unlockWallet", req);
  }

  deposit(req: DepositRequest = {}): Promise<DepositResult> {
    return this.callRaw<DepositResult>("deposit", req);
  }

  receive(req: ReceiveRequest): Promise<ReceiveResult> {
    return this.callRaw<ReceiveResult>("receive", req);
  }

  send(req: SendRequest): Promise<SendResult> {
    return this.callRaw<SendResult>("send", req);
  }

  list(req: ListRequest = {}): Promise<ListResult> {
    return this.callRaw<ListResult>("list", req);
  }

  exit(req: ExitRequest): Promise<ExitResult> {
    return this.callRaw<ExitResult>("exit", req);
  }

  exitStatus(req: ExitStatusRequest): Promise<ExitStatusResult> {
    return this.callRaw<ExitStatusResult>("exitStatus", req);
  }

  async callRaw<T = unknown>(
    method: string,
    params: unknown = {},
  ): Promise<T> {
    await this.ensureLoaded();

    const globalWallet = globalThis as typeof globalThis & {
      walletdkCall?: (method: string, params?: unknown) => Promise<T>;
    };

    if (typeof globalWallet.walletdkCall !== "function") {
      throw new WalletDKError("walletdk wasm runtime is not ready");
    }

    try {
      const result = await globalWallet.walletdkCall(method, params);
      this.emit({
        type: "sqliteOpenResults",
        payload: (globalThis as typeof globalThis & {
          sqliteBridgeOpenResults?: unknown;
        }).sqliteBridgeOpenResults,
      });

      return result;
    } catch (err) {
      throw new WalletDKError(errorMessage(err));
    }
  }

  subscribe(listener: WalletDKListener): () => void {
    this.listeners.add(listener);

    return () => {
      this.listeners.delete(listener);
    };
  }

  private ensureLoaded(): Promise<void> {
    if (!this.loadPromise) {
      this.loadPromise = this.loadRuntime();
    }

    return this.loadPromise;
  }

  private async loadRuntime() {
    if (typeof walletdkCall() === "function") {
      return;
    }

    await loadScript("sqlite-bridge.js");
    await loadScript("wasm_exec.js");

    const goCtor = (globalThis as typeof globalThis & {
      Go?: new () => {
        importObject: WebAssembly.Imports;
        run(instance: WebAssembly.Instance): Promise<void>;
      };
    }).Go;
    if (!goCtor) {
      throw new WalletDKError("Go WASM runtime did not load");
    }

    const go = new goCtor();
    const result = await instantiateWasm(go.importObject);
    const runPromise = go.run(result.instance);
    runPromise.catch((err) => {
      this.emit({
        type: "log",
        payload: { level: "error", message: errorMessage(err) },
      });
    });

    await waitForReadyEvent();
  }

  private emit(event: WalletDKEvent) {
    for (const listener of this.listeners) {
      listener(event);
    }
  }
}

export class WorkerWalletDKClient implements WalletDKClient {
  private readonly worker: Worker;
  private readonly pending = new Map<number, PendingCall>();
  private readonly listeners = new Set<WalletDKListener>();
  private nextRequestID = 1;

  constructor(options: WasmWalletDKClientOptions = {}) {
    const workerURL = options.workerURL ?? new URL(
      "walletdk-worker.js",
      document.baseURI,
    ).href;

    this.worker = new Worker(workerURL);
    this.worker.onmessage = (event) => this.handleMessage(event.data);
    this.worker.onerror = (event) => {
      this.rejectAll(new WalletDKError(
        event.message || "walletdk worker error",
        "worker_error",
      ));
    };
  }

  ready(): Promise<void> {
    return this.callRaw("$ready").then(() => undefined);
  }

  start(config: RuntimeConfig): Promise<WalletInfo> {
    return this.callRaw<WalletInfo>("start", config);
  }

  async stop(): Promise<void> {
    await this.callRaw("stop");
  }

  getInfo(): Promise<WalletInfo> {
    return this.callRaw<WalletInfo>("getInfo");
  }

  status(): Promise<WalletStatus> {
    return this.callRaw<WalletStatus>("status");
  }

  balance(): Promise<Balance> {
    // getRawBalance returns the daemon's per-category breakdown (VTXO,
    // boarding confirmed/unconfirmed, on-chain). The flat "balance" verb
    // collapses everything into a single confirmed figure, which cannot
    // represent a pending boarding deposit or an honest composition.
    return this.callRaw<Balance>("getRawBalance");
  }

  createWallet(req: CreateWalletRequest): Promise<CreateWalletResult> {
    return this.callRaw<CreateWalletResult>("createWallet", req);
  }

  unlockWallet(req: UnlockWalletRequest): Promise<UnlockWalletResult> {
    return this.callRaw<UnlockWalletResult>("unlockWallet", req);
  }

  deposit(req: DepositRequest = {}): Promise<DepositResult> {
    return this.callRaw<DepositResult>("deposit", req);
  }

  receive(req: ReceiveRequest): Promise<ReceiveResult> {
    return this.callRaw<ReceiveResult>("receive", req);
  }

  send(req: SendRequest): Promise<SendResult> {
    return this.callRaw<SendResult>("send", req);
  }

  list(req: ListRequest = {}): Promise<ListResult> {
    return this.callRaw<ListResult>("list", req);
  }

  exit(req: ExitRequest): Promise<ExitResult> {
    return this.callRaw<ExitResult>("exit", req);
  }

  exitStatus(req: ExitStatusRequest): Promise<ExitStatusResult> {
    return this.callRaw<ExitStatusResult>("exitStatus", req);
  }

  callRaw<T = unknown>(method: string, params: unknown = {}): Promise<T> {
    const id = this.nextRequestID++;

    const promise = new Promise<T>((resolve, reject) => {
      this.pending.set(id, {
        resolve: (value) => resolve(value as T),
        reject,
      });
    });

    this.worker.postMessage({ id, method, params });

    return promise;
  }

  subscribe(listener: WalletDKListener): () => void {
    this.listeners.add(listener);

    return () => {
      this.listeners.delete(listener);
    };
  }

  private handleMessage(message: unknown) {
    if (!message || typeof message !== "object") {
      return;
    }

    const data = message as {
      id?: number;
      ok?: boolean;
      result?: unknown;
      error?: string;
      event?: WalletDKEvent;
    };

    if (data.event) {
      this.emit(data.event);

      return;
    }

    if (typeof data.id !== "number") {
      return;
    }

    const pending = this.pending.get(data.id);
    if (!pending) {
      return;
    }
    this.pending.delete(data.id);

    if (data.ok) {
      pending.resolve(data.result);

      return;
    }

    pending.reject(new WalletDKError(
      data.error || "walletdk request failed",
    ));
  }

  private emit(event: WalletDKEvent) {
    if (event.type === "sqliteOpenResults") {
      const globalState = globalThis as typeof globalThis & {
        sqliteBridgeOpenResults?: unknown;
      };
      globalState.sqliteBridgeOpenResults = event.payload;
    }

    for (const listener of this.listeners) {
      listener(event);
    }
  }

  private rejectAll(err: Error) {
    for (const pending of this.pending.values()) {
      pending.reject(err);
    }
    this.pending.clear();
  }
}

function loadScript(src: string): Promise<void> {
  const existing = document.querySelector(`script[src="${src}"]`);
  if (existing) {
    return Promise.resolve();
  }

  return new Promise((resolve, reject) => {
    const script = document.createElement("script");
    script.src = src;
    script.async = false;
    script.onload = () => resolve();
    script.onerror = () => reject(new WalletDKError(
      `failed to load ${src}`,
      "asset_load_failed",
    ));
    document.head.append(script);
  });
}

function waitForReadyEvent(): Promise<void> {
  if (typeof walletdkCall() === "function") {
    return Promise.resolve();
  }

  return new Promise((resolve) => {
    globalThis.addEventListener("walletdk-ready", () => resolve(), {
      once: true,
    });
  });
}

function walletdkCall() {
  return (globalThis as typeof globalThis & {
    walletdkCall?: (method: string, params?: unknown) => Promise<unknown>;
  }).walletdkCall;
}

async function instantiateWasm(importObject: WebAssembly.Imports) {
  if ("DecompressionStream" in globalThis) {
    try {
      return await instantiateCompressedWasm(importObject);
    } catch (err) {
      console.warn(`compressed wasm load failed: ${errorMessage(err)}`);
    }
  }

  return instantiateRawWasm(importObject);
}

async function instantiateCompressedWasm(importObject: WebAssembly.Imports) {
  const response = await fetch("walletdk.wasm.gz");
  if (!response.ok) {
    throw new WalletDKError("walletdk compressed wasm artifact not found");
  }

  const body = response.body;
  if (!body) {
    throw new WalletDKError("walletdk compressed wasm response is empty");
  }

  const stream = body.pipeThrough(new DecompressionStream("gzip"));
  const bytes = await new Response(stream).arrayBuffer();

  return WebAssembly.instantiate(bytes, importObject);
}

async function instantiateRawWasm(importObject: WebAssembly.Imports) {
  const response = await fetch("walletdk.wasm");
  if (!response.ok) {
    throw new WalletDKError("walletdk wasm artifact not found");
  }

  return WebAssembly.instantiateStreaming(response, importObject);
}

function errorMessage(err: unknown): string {
  if (err instanceof Error && err.message) {
    return err.message;
  }
  if (typeof err === "string") {
    return err;
  }

  return JSON.stringify(err);
}

export {
  clearPasskeyWrap,
  createPasskeyWrap,
  hasPasskeyWrap,
  loadPasskeyWrap,
  supportsPasskeyPrf,
  unwrapPasskeyPassword,
} from "./passkey";
export type {
  PasskeyWrapOptions,
  PasskeyWrapRecord,
} from "./passkey";
