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
  OpenWalletFromPasskeyRequest,
  OpenWalletFromPasskeyResult,
  PrepareSendResult,
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
  WalletState,
  WalletStatus,
} from '@lightninglabs/walletdk-core';

type PendingCall = {
  resolve: (value: unknown) => void;
  reject: (err: Error) => void;
};

// ActivityHandle is the pull-based subscription the 803 bridge's `subscribe`
// verb resolves to: next() yields the next activity entry (or null at end of
// stream) and close() cancels it.
type ActivityHandle = {
  next: () => Promise<unknown>;
  close: () => unknown;
};

// MobileConfig is the flat, snake_case config the walletdk mobile facade's
// `start` verb decodes (sdk/walletdk/mobile.mobileConfig). The browser bridge
// forwards it verbatim to mobile.Start.
type MobileConfig = {
  data_dir?: string;
  network?: string;
  debug_level?: string;
  wallet_type?: string;
  wallet_esplora_url?: string;
  server_address?: string;
  server_transport?: string;
  server_insecure?: boolean;
  swap_server_address?: string;
  swap_server_transport?: string;
  swap_server_insecure?: boolean;
  swap_database_file_name?: string;
};

// toMobileConfig maps the demo's RuntimeConfig onto the flat config the mobile
// facade expects. The embedded daemon runs in the browser and reaches the Ark
// operator and swap server over grpc-gateway REST — a browser cannot speak
// native gRPC — so the gateway URLs become REST server addresses. The Ark and
// mailbox gateways address the same edge server, so the *MailboxGatewayURL
// fields fold into the single server address the facade exposes. Only the
// lightweight Esplora-backed wallet runs under wasm.
function toMobileConfig(config: RuntimeConfig): MobileConfig {
  const out: MobileConfig = {
    network: config.network,
    data_dir: config.dataDir,
    debug_level: config.debugLevel,
    wallet_type: 'lwwallet',
    wallet_esplora_url: config.walletEsploraURL,
    server_address: config.arkGatewayURL,
    server_transport: 'rest',
    server_insecure: config.serverInsecure,
  };

  // Leaving the swap server address unset disables the swap subsystem, so omit
  // every swap field when the host asked to run without swaps.
  if (!config.disableSwaps) {
    out.swap_server_address = config.swapServerGatewayURL;
    out.swap_server_transport = 'rest';
    out.swap_server_insecure = config.swapServerInsecure;
    out.swap_database_file_name = config.swapDatabaseFileName;
  }

  return out;
}

// withWalletReady backfills the WalletReady predicate the 803 facade omits: it
// marshals walletdk.Info, whose WalletReady() is a Go method rather than a
// field, so the JSON lacks it. Mirror the Go rule (ready iff WalletState ==
// Ready) so hosts see the same convenience flag the old bridge provided.
function withWalletReady(info: WalletInfo): WalletInfo {
  if (info.WalletReady === undefined && info.WalletState !== undefined) {
    return { ...info, WalletReady: info.WalletState === WalletState.Ready };
  }

  return info;
}

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
  private activityHandle: ActivityHandle | null = null;

  constructor(_options: WasmWalletDKClientOptions = {}) {
    globalThis.addEventListener('walletdk-ready', () => {
      this.emit({ type: 'runtimeReady' });
    });
  }

  ready(): Promise<void> {
    return this.ensureLoaded();
  }

  // start boots the embedded daemon and returns the post-boot WalletInfo. The
  // 803 bridge's start verb resolves null (it only calls mobile.Start), so the
  // client fetches getInfo afterwards — the old bridge returned info inline and
  // the React provider derives the runtime phase from it.
  async start(config: RuntimeConfig): Promise<WalletInfo> {
    await this.callRaw('start', toMobileConfig(config));

    return this.getInfo();
  }

  async stop(): Promise<void> {
    await this.callRaw('stop');
    this.emit({ type: 'runtimeStopped' });
  }

  async getInfo(): Promise<WalletInfo> {
    return withWalletReady(await this.callRaw<WalletInfo>('getInfo'));
  }

  status(): Promise<WalletStatus> {
    return this.callRaw<WalletStatus>('status');
  }

  balance(): Promise<Balance> {
    // walletdkrpc.Balance: confirmed_sat (spendable VTXO), pending_in_sat,
    // pending_out_sat — same surface as darepocli balance.
    return this.callRaw<Balance>('balance');
  }

  createWallet(req: CreateWalletRequest): Promise<CreateWalletResult> {
    return this.callRaw<CreateWalletResult>('createWallet', req);
  }

  unlockWallet(req: UnlockWalletRequest): Promise<UnlockWalletResult> {
    return this.callRaw<UnlockWalletResult>('unlockWallet', req);
  }

  openWalletFromPasskey(
    req: OpenWalletFromPasskeyRequest,
  ): Promise<OpenWalletFromPasskeyResult> {
    return this.callRaw<OpenWalletFromPasskeyResult>(
      'openWalletFromPasskey',
      req,
    );
  }

  deposit(req: DepositRequest = {}): Promise<DepositResult> {
    return this.callRaw<DepositResult>('deposit', req);
  }

  receive(req: ReceiveRequest): Promise<ReceiveResult> {
    return this.callRaw<ReceiveResult>('receive', req);
  }

  // send composes the facade's two-step prepare/dispatch into one call: it
  // quotes the payment with prepareSend, then dispatches the returned
  // single-use SendIntentID with sendPrepared. The prepare-time PaymentHash is
  // folded into the result so the UI can echo it (sendPrepared omits it).
  async send(req: SendRequest): Promise<SendResult> {
    const prepared = await this.callRaw<PrepareSendResult>('prepareSend', req);
    const result = await this.callRaw<SendResult>('sendPrepared', {
      SendIntentID: prepared.SendIntentID,
    });

    return {
      ...result,
      PaymentHash: result.PaymentHash ?? prepared.PaymentHash,
    };
  }

  list(req: ListRequest = {}): Promise<ListResult> {
    return this.callRaw<ListResult>('list', req);
  }

  exit(req: ExitRequest): Promise<ExitResult> {
    return this.callRaw<ExitResult>('exit', req);
  }

  exitStatus(req: ExitStatusRequest): Promise<ExitStatusResult> {
    return this.callRaw<ExitStatusResult>('exitStatus', req);
  }

  async callRaw<T = unknown>(method: string, params: unknown = {}): Promise<T> {
    await this.ensureLoaded();

    const globalWallet = globalThis as typeof globalThis & {
      walletdkCall?: (method: string, params?: unknown) => Promise<T>;
    };

    if (typeof globalWallet.walletdkCall !== 'function') {
      throw new WalletDKError('walletdk wasm runtime is not ready');
    }

    try {
      // Format the current timestamp as a string in the format "YYYY-MM-DD HH:MM:SS".
      const ts = () =>
        new Date().toISOString().split('T').join(' ').slice(0, -1);
      // Log RPC call request/response payloads for debugging purposes.
      console.log(`${ts()} Executing ${method}:`, params);
      const result = await globalWallet.walletdkCall(method, params);
      console.log(`${ts()} Executed ${method} result:`, result);
      this.emit({
        type: 'sqliteOpenResults',
        payload: (
          globalThis as typeof globalThis & {
            sqliteBridgeOpenResults?: unknown;
          }
        ).sqliteBridgeOpenResults,
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

  // startActivity opens the facade's pull-based activity stream and pumps each
  // entry to subscribers as an 'activity' event. The old bridge pushed a
  // 'walletdk-activity' DOM event; the 803 bridge hands back a subscription
  // handle instead, so the client drives the loop. Idempotent: a second call
  // while a stream is open is a no-op.
  async startActivity(opts: { includeExisting?: boolean } = {}): Promise<void> {
    await this.ensureLoaded();
    if (this.activityHandle) {
      return;
    }

    const call = walletdkCall();
    if (typeof call !== 'function') {
      throw new WalletDKError('walletdk wasm runtime is not ready');
    }

    const handle = (await call('subscribe', {
      includeExisting: opts.includeExisting ?? false,
    })) as ActivityHandle;
    this.activityHandle = handle;
    void this.pumpActivity(handle);
  }

  stopActivity(): void {
    const handle = this.activityHandle;
    this.activityHandle = null;
    handle?.close();
  }

  // pumpActivity drains the subscription handle until it ends (next() resolves
  // null) or stopActivity swaps the handle out from under it.
  private async pumpActivity(handle: ActivityHandle): Promise<void> {
    try {
      for (
        let entry = await handle.next();
        entry !== null && this.activityHandle === handle;
        entry = await handle.next()
      ) {
        this.emit({ type: 'activity', payload: entry });
      }
    } catch (err) {
      this.emit({
        type: 'log',
        payload: { level: 'error', message: errorMessage(err) },
      });
    }
  }

  private ensureLoaded(): Promise<void> {
    if (!this.loadPromise) {
      this.loadPromise = this.loadRuntime();
    }

    return this.loadPromise;
  }

  private async loadRuntime() {
    if (typeof walletdkCall() === 'function') {
      return;
    }

    await loadScript('sqlite-bridge.js');
    await loadScript('wasm_exec.js');

    const goCtor = (
      globalThis as typeof globalThis & {
        Go?: new () => {
          importObject: WebAssembly.Imports;
          run(instance: WebAssembly.Instance): Promise<void>;
        };
      }
    ).Go;
    if (!goCtor) {
      throw new WalletDKError('Go WASM runtime did not load');
    }

    const go = new goCtor();
    const result = await instantiateWasm(go.importObject);
    const runPromise = go.run(result.instance);
    runPromise.catch((err) => {
      this.emit({
        type: 'log',
        payload: { level: 'error', message: errorMessage(err) },
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
    const workerURL =
      options.workerURL ?? new URL('walletdk-worker.js', document.baseURI).href;

    this.worker = new Worker(workerURL);
    this.worker.onmessage = (event) => this.handleMessage(event.data);
    this.worker.onerror = (event) => {
      this.rejectAll(
        new WalletDKError(
          event.message || 'walletdk worker error',
          'worker_error',
        ),
      );
    };
  }

  ready(): Promise<void> {
    return this.callRaw('$ready').then(() => undefined);
  }

  // start boots the embedded daemon and returns the post-boot WalletInfo. The
  // 803 bridge's start verb resolves null (it only calls mobile.Start), so the
  // client fetches getInfo afterwards — the old bridge returned info inline and
  // the React provider derives the runtime phase from it.
  async start(config: RuntimeConfig): Promise<WalletInfo> {
    await this.callRaw('start', toMobileConfig(config));

    return this.getInfo();
  }

  async stop(): Promise<void> {
    await this.callRaw('stop');
  }

  async getInfo(): Promise<WalletInfo> {
    return withWalletReady(await this.callRaw<WalletInfo>('getInfo'));
  }

  status(): Promise<WalletStatus> {
    return this.callRaw<WalletStatus>('status');
  }

  balance(): Promise<Balance> {
    // walletdkrpc.Balance: confirmed_sat (spendable VTXO), pending_in_sat,
    // pending_out_sat — same surface as darepocli balance.
    return this.callRaw<Balance>('balance');
  }

  createWallet(req: CreateWalletRequest): Promise<CreateWalletResult> {
    return this.callRaw<CreateWalletResult>('createWallet', req);
  }

  unlockWallet(req: UnlockWalletRequest): Promise<UnlockWalletResult> {
    return this.callRaw<UnlockWalletResult>('unlockWallet', req);
  }

  openWalletFromPasskey(
    req: OpenWalletFromPasskeyRequest,
  ): Promise<OpenWalletFromPasskeyResult> {
    return this.callRaw<OpenWalletFromPasskeyResult>(
      'openWalletFromPasskey',
      req,
    );
  }

  deposit(req: DepositRequest = {}): Promise<DepositResult> {
    return this.callRaw<DepositResult>('deposit', req);
  }

  receive(req: ReceiveRequest): Promise<ReceiveResult> {
    return this.callRaw<ReceiveResult>('receive', req);
  }

  // send composes the facade's two-step prepare/dispatch into one call: it
  // quotes the payment with prepareSend, then dispatches the returned
  // single-use SendIntentID with sendPrepared. The prepare-time PaymentHash is
  // folded into the result so the UI can echo it (sendPrepared omits it).
  async send(req: SendRequest): Promise<SendResult> {
    const prepared = await this.callRaw<PrepareSendResult>('prepareSend', req);
    const result = await this.callRaw<SendResult>('sendPrepared', {
      SendIntentID: prepared.SendIntentID,
    });

    return {
      ...result,
      PaymentHash: result.PaymentHash ?? prepared.PaymentHash,
    };
  }

  list(req: ListRequest = {}): Promise<ListResult> {
    return this.callRaw<ListResult>('list', req);
  }

  exit(req: ExitRequest): Promise<ExitResult> {
    return this.callRaw<ExitResult>('exit', req);
  }

  exitStatus(req: ExitStatusRequest): Promise<ExitStatusResult> {
    return this.callRaw<ExitStatusResult>('exitStatus', req);
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

  // startActivity asks the worker to open the activity stream. The 803 bridge's
  // subscription handle holds JS callbacks that cannot cross postMessage, so the
  // worker drives the pull loop and forwards each entry as an 'activity' event
  // message instead of returning the handle.
  async startActivity(opts: { includeExisting?: boolean } = {}): Promise<void> {
    await this.callRaw('$startActivity', {
      includeExisting: opts.includeExisting ?? false,
    });
  }

  stopActivity(): void {
    void this.callRaw('$stopActivity').catch(() => undefined);
  }

  private handleMessage(message: unknown) {
    if (!message || typeof message !== 'object') {
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

    if (typeof data.id !== 'number') {
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

    pending.reject(new WalletDKError(data.error || 'walletdk request failed'));
  }

  private emit(event: WalletDKEvent) {
    if (event.type === 'sqliteOpenResults') {
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
    const script = document.createElement('script');
    script.src = src;
    script.async = false;
    script.onload = () => resolve();
    script.onerror = () =>
      reject(new WalletDKError(`failed to load ${src}`, 'asset_load_failed'));
    document.head.append(script);
  });
}

function waitForReadyEvent(): Promise<void> {
  if (typeof walletdkCall() === 'function') {
    return Promise.resolve();
  }

  return new Promise((resolve) => {
    globalThis.addEventListener('walletdk-ready', () => resolve(), {
      once: true,
    });
  });
}

function walletdkCall() {
  return (
    globalThis as typeof globalThis & {
      walletdkCall?: (method: string, params?: unknown) => Promise<unknown>;
    }
  ).walletdkCall;
}

async function instantiateWasm(importObject: WebAssembly.Imports) {
  if ('DecompressionStream' in globalThis) {
    try {
      return await instantiateCompressedWasm(importObject);
    } catch (err) {
      console.warn(`compressed wasm load failed: ${errorMessage(err)}`);
    }
  }

  return instantiateRawWasm(importObject);
}

async function instantiateCompressedWasm(importObject: WebAssembly.Imports) {
  const response = await fetch('walletdk.wasm.gz');
  if (!response.ok) {
    throw new WalletDKError('walletdk compressed wasm artifact not found');
  }

  const body = response.body;
  if (!body) {
    throw new WalletDKError('walletdk compressed wasm response is empty');
  }

  const stream = body.pipeThrough(new DecompressionStream('gzip'));
  const bytes = await new Response(stream).arrayBuffer();

  return WebAssembly.instantiate(bytes, importObject);
}

async function instantiateRawWasm(importObject: WebAssembly.Imports) {
  const response = await fetch('walletdk.wasm');
  if (!response.ok) {
    throw new WalletDKError('walletdk wasm artifact not found');
  }

  return WebAssembly.instantiateStreaming(response, importObject);
}

function errorMessage(err: unknown): string {
  if (err instanceof Error && err.message) {
    return err.message;
  }
  if (typeof err === 'string') {
    return err;
  }

  return JSON.stringify(err);
}

// WasmOpenWalletResult is the PascalCase shape returned by the Go wasm
// openWalletFromPasskey method, mirroring the Go SDK OpenWalletResult.
type WasmOpenWalletResult = {
  Imported: boolean;
  // Mnemonic is nullable because Go marshals a nil slice as JSON null on the
  // unlock / already-ready paths where no mnemonic is returned.
  Mnemonic?: string[] | null;
  IdentityPubKey?: string;
};

export {
  assertPasskeyPrf,
  registerPasskeyWallet,
  supportsPasskeyPrf,
} from './passkey';
export type { PasskeyAssertion } from './passkey';
