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
  UnlockWalletRequest,
  UnlockWalletResult,
  WalletDKClient,
  WalletInfo,
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
  | "wasm-ready"
  | "starting"
  | "started"
  | "stopped"
  | "error";

export type WalletDKReactState = {
  client: WalletDKClient;
  phase: RuntimePhase;
  error: string;
  info: WalletInfo | null;
  balance: Balance | null;
  activity: Entry[];
  start(config: RuntimeConfig): Promise<WalletInfo>;
  stop(): Promise<void>;
  refresh(): Promise<void>;
  createWallet(req: CreateWalletRequest): Promise<CreateWalletResult>;
  unlockWallet(req: UnlockWalletRequest): Promise<UnlockWalletResult>;
  deposit(req?: DepositRequest): Promise<string>;
  receive(req: ReceiveRequest): Promise<string>;
  send(req: SendRequest): Promise<string>;
};

const WalletDKContext = createContext<WalletDKReactState | null>(null);

export function WalletDKProvider({ children }: { children: ReactNode }) {
  const client = useMemo(() => createWasmWalletDKClient(), []);
  const [phase, setPhase] = useState<RuntimePhase>("loading");
  const [error, setError] = useState("");
  const [info, setInfo] = useState<WalletInfo | null>(null);
  const [balance, setBalance] = useState<Balance | null>(null);
  const [activity, setActivity] = useState<Entry[]>([]);

  useEffect(() => {
    let cancelled = false;

    client.ready().then(() => {
      if (!cancelled) {
        setPhase("wasm-ready");
      }
    }).catch((err) => {
      if (!cancelled) {
        setError(errorMessage(err));
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
        setPhase((current) => current === "loading" ? "wasm-ready" : current);
      }
    });
  }, [client]);

  const refresh = useCallback(async () => {
    const nextInfo = await client.getInfo();
    setInfo(nextInfo);

    const nextBalance = await client.balance();
    setBalance(nextBalance);

    const rows = await client.list({ view: "activity", pendingOnly: false });
    setActivity(activityEntries(rows));
  }, [client]);

  const start = useCallback(async (config: RuntimeConfig) => {
    setPhase("starting");
    setError("");

    const nextInfo = await client.start(config);
    setInfo(nextInfo);
    setPhase("started");

    try {
      await refresh();
    } catch {
      // A locked or empty wallet can fail balance/list until bootstrap.
    }

    return nextInfo;
  }, [client, refresh]);

  const stop = useCallback(async () => {
    await client.stop();
    setInfo(null);
    setBalance(null);
    setActivity([]);
    setPhase("stopped");
  }, [client]);

  const createWallet = useCallback(async (req: CreateWalletRequest) => {
    const result = await client.createWallet(req);
    setInfo((current) => ({
      ...(current || {}),
      IdentityPubKey: result.IdentityPubKey,
      WalletReady: true,
    }));
    await refresh();

    return result;
  }, [client, refresh]);

  const unlockWallet = useCallback(async (req: UnlockWalletRequest) => {
    const result = await client.unlockWallet(req);
    setInfo((current) => ({
      ...(current || {}),
      IdentityPubKey: result.IdentityPubKey,
      WalletReady: true,
    }));
    await refresh();

    return result;
  }, [client, refresh]);

  const deposit = useCallback(async (req: DepositRequest = {}) => {
    const result = await client.deposit(req);
    await refresh();

    return result.Address;
  }, [client, refresh]);

  const receive = useCallback(async (req: ReceiveRequest) => {
    const result = await client.receive(req);
    await refresh();

    return result.Invoice;
  }, [client, refresh]);

  const send = useCallback(async (req: SendRequest) => {
    const result = await client.send(req);
    await refresh();

    return result.PaymentHash || result.Entry?.ID || "";
  }, [client, refresh]);

  const value = useMemo<WalletDKReactState>(() => ({
    activity,
    balance,
    client,
    createWallet,
    deposit,
    error,
    info,
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
    client,
    createWallet,
    deposit,
    error,
    info,
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
  const { client, phase, error, info, start, stop, refresh } = useWalletDK();

  return { client, phase, error, info, start, stop, refresh };
}

export function useWalletBootstrap() {
  const { createWallet, unlockWallet, info } = useWalletDK();

  return { createWallet, unlockWallet, info };
}

export function useWalletBalance() {
  const { balance, refresh } = useWalletDK();

  return { balance, refresh };
}

export function useWalletActivity() {
  const { activity, refresh } = useWalletDK();

  return { activity, refresh };
}

export function useDepositAddress() {
  const { deposit } = useWalletDK();

  return { deposit };
}

export function useReceive() {
  const { receive } = useWalletDK();

  return { receive };
}

export function useSend() {
  const { send } = useWalletDK();

  return { send };
}

function activityEntries(result: ListResult): Entry[] {
  return result.Activity?.Entries || [];
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
