import { RuntimeConfig } from "@lightninglabs/walletdk-core";

// RuntimeForm is the fully-populated runtime config the connect/settings forms
// edit (every field required so inputs are always controlled).
export type RuntimeForm = Required<RuntimeConfig>;

// RuntimeFieldSetter updates a single field of the runtime form, preserving the
// value type of that field (string or boolean).
export type RuntimeFieldSetter = <K extends keyof RuntimeForm>(
  key: K,
  value: RuntimeForm[K],
) => void;

// signetDefaults are the default runtime gateways for the signet test network,
// preserved from the original demo.
export const signetDefaults: RuntimeForm = {
  network: "signet",
  dataDir: "/walletdk-demo",
  arkGatewayURL: "https://arkd-signet-rest.testnet.lightningcluster.com",
  mailboxGatewayURL: "https://arkd-signet-rest.testnet.lightningcluster.com",
  walletEsploraURL: "https://mempool-signet.testnet.lightningcluster.com/api",
  swapServerGatewayURL: "https://swapd-signet-rest.testnet.lightningcluster.com",
  swapMailboxGatewayURL: "https://swapd-signet-rest.testnet.lightningcluster.com",
  swapDatabaseFileName: "/walletdk-swaps.db",
  serverInsecure: false,
  swapServerInsecure: false,
  disableSwaps: false,
  debugLevel: "info",
};

// NETWORKS are the selectable runtime networks. Mainnet is intentionally
// excluded - this build targets test networks only.
export const NETWORKS = ["signet", "testnet", "regtest"] as const;

// hostname extracts the host from a URL for compact display, falling back to
// the raw value when it is not a parseable URL.
export function hostname(value: string): string {
  try {
    return new URL(value).hostname;
  } catch {
    return value;
  }
}
