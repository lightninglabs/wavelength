import {
  Balance,
  CreateWalletRequest,
  CreateWalletResult,
  DepositRequest,
  Entry,
  ListResult,
  ReceiveRequest,
  RuntimeConfig,
  SendRequest,
  SendResult,
  UnlockWalletRequest,
  UnlockWalletResult,
  WalletDKClient,
  WalletInfo,
  WalletState,
} from "@lightninglabs/walletdk-core";
import { createWasmWalletDKClient } from "@lightninglabs/walletdk-wasm-web";
import {
  ReactNode,
  createContext,
  useCallback,
  useContext,
  useEffect,
  useMemo,
  useState,
} from "react";

export type RuntimePhase =
  | "loading"
  | "runtimeReady"
  | "starting"
  | "needsWallet"
  | "locked"
  | "syncing"
  | "ready"
  | "stopping"
  | "stopped"
  | "error";

export type WalletOperation =
  | "runtime"
  | "refresh"
  | "createWallet"
  | "unlockWallet"
  | "deposit"
  | "receive"
  | "send";

export type OperationState = {
  busy: boolean;
  error: string;
};

export type WalletDKReactState = {
  client: WalletDKClient;
  phase: RuntimePhase;
  error: string;
  info: WalletInfo | null;
  balance: Balance | null;
  activity: Entry[];
  operations: Record<WalletOperation, OperationState>;
  start(config: RuntimeConfig): Promise<WalletInfo>;
  stop(): Promise<void>;
  refresh(): Promise<void>;
  createWallet(req: CreateWalletRequest): Promise<CreateWalletResult>;
  unlockWallet(req: UnlockWalletRequest): Promise<UnlockWalletResult>;
  deposit(req?: DepositRequest): Promise<string>;
  receive(req: ReceiveRequest): Promise<string>;
  send(req: SendRequest): Promise<SendResult>;
  clearOperationError(operation: WalletOperation): void;
};

const WalletDKContext = createContext<WalletDKReactState | null>(null);

const defaultOperations: Record<WalletOperation, OperationState> = {
  runtime: { busy: false, error: "" },
  refresh: { busy: false, error: "" },
  createWallet: { busy: false, error: "" },
  unlockWallet: { busy: false, error: "" },
  deposit: { busy: false, error: "" },
  receive: { busy: false, error: "" },
  send: { busy: false, error: "" },
};

export function WalletDKProvider({ children }: { children: ReactNode }) {
  const client = useMemo(() => createWasmWalletDKClient(), []);
  const [phase, setPhase] = useState<RuntimePhase>("loading");
  const [error, setError] = useState("");
  const [info, setInfo] = useState<WalletInfo | null>(null);
  const [balance, setBalance] = useState<Balance | null>(null);
  const [activity, setActivity] = useState<Entry[]>([]);
  const [operations, setOperations] = useState(defaultOperations);

  useEffect(() => {
    let cancelled = false;

    client.ready().then(() => {
      if (!cancelled) {
        setPhase("runtimeReady");
      }
    }).catch((err) => {
      if (!cancelled) {
        const message = errorMessage(err);
        setError(message);
        setPhase("error");
      }
    });

    return () => {
      cancelled = true;
    };
  }, [client]);

  useEffect(() => {
    return client.subscribe((event) => {
      if (event.type === "runtimeReady") {
        setPhase((current) => {
          return current === "loading" ? "runtimeReady" : current;
        });
      }
    });
  }, [client]);

  const setOperation = useCallback((
    operation: WalletOperation,
    patch: Partial<OperationState>,
  ) => {
    setOperations((current) => ({
      ...current,
      [operation]: {
        ...current[operation],
        ...patch,
      },
    }));
  }, []);

  const runOperation = useCallback(async <T,>(
    operation: WalletOperation,
    fn: () => Promise<T>,
  ): Promise<T> => {
    setOperation(operation, { busy: true, error: "" });

    try {
      return await fn();
    } catch (err) {
      const message = errorMessage(err);
      setOperation(operation, { error: message });
      throw err;
    } finally {
      setOperation(operation, { busy: false });
    }
  }, [setOperation]);

  const refresh = useCallback(async () => {
    return runOperation("refresh", async () => {
      const nextInfo = await client.getInfo();
      setInfo(nextInfo);
      setPhase(phaseFromInfo(nextInfo));

      const nextBalance = await client.balance();
      setBalance(nextBalance);

      const rows = await client.list({
        view: "activity",
        pendingOnly: false,
      });
      setActivity(activityEntries(rows));
    });
  }, [client, runOperation]);

  const start = useCallback(async (config: RuntimeConfig) => {
    setPhase("starting");
    setError("");

    return runOperation("runtime", async () => {
      const nextInfo = await client.start(config);
      setInfo(nextInfo);
      setPhase(phaseFromInfo(nextInfo));

      try {
        await refresh();
      } catch {
        // A locked or empty wallet can fail balance/list until bootstrap.
      }

      return nextInfo;
    });
  }, [client, refresh, runOperation]);

  const stop = useCallback(async () => {
    setPhase("stopping");

    return runOperation("runtime", async () => {
      await client.stop();
      setInfo(null);
      setBalance(null);
      setActivity([]);
      setPhase("stopped");
    });
  }, [client, runOperation]);

  const createWallet = useCallback(async (req: CreateWalletRequest) => {
    return runOperation("createWallet", async () => {
      const result = await client.createWallet(req);
      setInfo((current) => ({
        ...(current || {}),
        IdentityPubKey: result.IdentityPubKey,
        WalletState: WalletState.Ready,
        WalletReady: true,
      }));
      setPhase("ready");
      await refresh();

      return result;
    });
  }, [client, refresh, runOperation]);

  const unlockWallet = useCallback(async (req: UnlockWalletRequest) => {
    return runOperation("unlockWallet", async () => {
      const result = await client.unlockWallet(req);
      setInfo((current) => ({
        ...(current || {}),
        IdentityPubKey: result.IdentityPubKey,
        WalletState: WalletState.Ready,
        WalletReady: true,
      }));
      setPhase("ready");
      await refresh();

      return result;
    });
  }, [client, refresh, runOperation]);

  const deposit = useCallback(async (req: DepositRequest = {}) => {
    return runOperation("deposit", async () => {
      const result = await client.deposit(req);
      await refresh();

      return result.Address;
    });
  }, [client, refresh, runOperation]);

  const receive = useCallback(async (req: ReceiveRequest) => {
    return runOperation("receive", async () => {
      const result = await client.receive(req);
      await refresh();

      return result.Invoice;
    });
  }, [client, refresh, runOperation]);

  const send = useCallback(async (req: SendRequest) => {
    return runOperation("send", async () => {
      const result = await client.send(req);
      await refresh();

      return result;
    });
  }, [client, refresh, runOperation]);

  const clearOperationError = useCallback((operation: WalletOperation) => {
    setOperation(operation, { error: "" });
  }, [setOperation]);

  const value = useMemo<WalletDKReactState>(() => ({
    activity,
    balance,
    clearOperationError,
    client,
    createWallet,
    deposit,
    error,
    info,
    operations,
    phase,
    receive,
    refresh,
    send,
    start,
    stop,
    unlockWallet,
  }), [
    activity,
    balance,
    clearOperationError,
    client,
    createWallet,
    deposit,
    error,
    info,
    operations,
    phase,
    receive,
    refresh,
    send,
    start,
    stop,
    unlockWallet,
  ]);

  return (
    <WalletDKContext.Provider value={value}>
      {children}
    </WalletDKContext.Provider>
  );
}

export function useWalletDK(): WalletDKReactState {
  const value = useContext(WalletDKContext);
  if (!value) {
    throw new Error("useWalletDK must be used inside WalletDKProvider");
  }

  return value;
}

export function useWalletRuntime() {
  const {
    client,
    error,
    info,
    operations,
    phase,
    refresh,
    start,
    stop,
  } = useWalletDK();

  return { client, error, info, operations, phase, refresh, start, stop };
}

export function useWalletBootstrap() {
  const { createWallet, info, operations, unlockWallet } = useWalletDK();

  return { createWallet, info, operations, unlockWallet };
}

export function useWalletBalance() {
  const { balance, operations, refresh } = useWalletDK();

  return { balance, operations, refresh };
}

export function useWalletActivity() {
  const { activity, operations, refresh } = useWalletDK();

  return { activity, operations, refresh };
}

export function useDepositAddress() {
  const { deposit, operations } = useWalletDK();

  return { deposit, operations };
}

export function useReceive() {
  const { operations, receive } = useWalletDK();

  return { operations, receive };
}

export function useSend() {
  const { operations, send } = useWalletDK();

  return { operations, send };
}

function activityEntries(result: ListResult): Entry[] {
  return result.Activity?.Entries || [];
}

function phaseFromInfo(info: WalletInfo): RuntimePhase {
  if (info.WalletReady || info.WalletState === WalletState.Ready) {
    return "ready";
  }

  switch (info.WalletState) {
  case WalletState.Locked:
    return "locked";

  case WalletState.Syncing:
    return "syncing";

  case WalletState.None:
  case WalletState.Unspecified:
  default:
    return "needsWallet";
  }
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
