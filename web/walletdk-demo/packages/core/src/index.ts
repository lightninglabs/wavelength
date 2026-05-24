export type WalletDKEventType =
  | "runtimeReady"
  | "runtimeStopped"
  | "walletState"
  | "activity"
  | "log"
  | "sqliteOpenResults";

export type WalletDKEvent = {
  type: WalletDKEventType;
  payload?: unknown;
};

export type RuntimeConfig = {
  network?: string;
  dataDir?: string;
  arkGatewayURL?: string;
  mailboxGatewayURL?: string;
  walletEsploraURL?: string;
  swapServerGatewayURL?: string;
  swapMailboxGatewayURL?: string;
  swapDatabaseFileName?: string;
  serverInsecure?: boolean;
  swapServerInsecure?: boolean;
  disableSwaps?: boolean;
  debugLevel?: string;
};

export type WalletInfo = {
  Version?: string;
  Commit?: string;
  Network?: string;
  BlockHeight?: number;
  ServerConnected?: boolean;
  WalletType?: string;
  WalletState?: number;
  WalletReady?: boolean;
  IdentityPubKey?: string;
};

export type WalletStatus = {
  Ready: boolean;
  Unlocked: boolean;
  Network: string;
  Balance: Balance;
  PendingCount: number;
};

export type Balance = {
  ConfirmedSat?: number;
  PendingInSat?: number;
  PendingOutSat?: number;
  BoardingConfirmedSat?: number;
  BoardingUnconfirmedSat?: number;
  VTXOBalanceSat?: number;
  TotalConfirmedSat?: number;
  OnchainWalletConfirmedSat?: number;
};

export type CreateWalletRequest = {
  password: string;
  mnemonic?: string[];
  seedPassphrase?: string;
};

export type CreateWalletResult = {
  Mnemonic: string[];
  EncipheredSeed?: number[];
  IdentityPubKey: string;
};

export type UnlockWalletRequest = {
  password: string;
};

export type UnlockWalletResult = {
  IdentityPubKey: string;
};

export type DepositRequest = {
  amountSatHint?: number;
};

export type DepositResult = {
  Address: string;
  Entry: Entry;
};

export type ReceiveRequest = {
  amountSat: number;
  memo?: string;
};

export type ReceiveResult = {
  Invoice: string;
  Entry?: Entry;
  PaymentHash?: string;
};

export type SendRequest = {
  invoice?: string;
  onchainAddress?: string;
  amountSat?: number;
  note?: string;
  maxFeeSat?: number;
  sweepAll?: boolean;
};

export type SendResult = {
  Entry?: Entry;
  ActualAmountSat?: number;
  PaymentHash?: string;
};

export type ListRequest = {
  view?: "activity" | "vtxos" | "onchain";
  pendingOnly?: boolean;
  limit?: number;
  offset?: number;
};

export type ListResult = {
  View: string;
  Activity?: {
    Entries: Entry[];
    Total: number;
  };
  VTXOs?: unknown;
  Onchain?: unknown;
};

export type Entry = {
  ID: string;
  Kind: "send" | "receive" | "deposit" | "exit" | string;
  Status: "pending" | "complete" | "failed" | string;
  AmountSat: number;
  FeeSat?: number;
  Counterparty?: string;
  CreatedAt?: string;
  UpdatedAt?: string;
  Note?: string;
  FailureReason?: string;
};

export type ExitRequest = {
  outpoint: string;
  destination?: string;
};

export type ExitResult = Record<string, unknown>;

export type ExitStatusRequest = {
  outpoint: string;
};

export type ExitStatusResult = Record<string, unknown>;

export type WalletDKListener = (event: WalletDKEvent) => void;

export interface WalletDKClient {
  ready(): Promise<void>;
  start(config: RuntimeConfig): Promise<WalletInfo>;
  stop(): Promise<void>;
  getInfo(): Promise<WalletInfo>;
  status(): Promise<WalletStatus>;
  balance(): Promise<Balance>;
  createWallet(req: CreateWalletRequest): Promise<CreateWalletResult>;
  unlockWallet(req: UnlockWalletRequest): Promise<UnlockWalletResult>;
  deposit(req?: DepositRequest): Promise<DepositResult>;
  receive(req: ReceiveRequest): Promise<ReceiveResult>;
  send(req: SendRequest): Promise<SendResult>;
  list(req?: ListRequest): Promise<ListResult>;
  exit(req: ExitRequest): Promise<ExitResult>;
  exitStatus(req: ExitStatusRequest): Promise<ExitStatusResult>;
  callRaw<T = unknown>(method: string, params?: unknown): Promise<T>;
  subscribe(listener: WalletDKListener): () => void;
}

export class WalletDKError extends Error {
  constructor(
    message: string,
    public readonly code = "walletdk_error",
  ) {
    super(message);
    this.name = "WalletDKError";
  }
}
