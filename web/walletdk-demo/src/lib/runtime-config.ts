import { RuntimeConfig } from "@lightninglabs/walletdk-core";

// NETWORKS are the selectable runtime networks. Mainnet is intentionally
// excluded - this build targets test networks only.
export const NETWORKS = ["signet", "testnet", "regtest"] as const;

// RuntimeNetwork is the demo's selectable network union. RuntimeConfig.network is
// a plain string, so RuntimeForm narrows it for controlled network pickers.
export type RuntimeNetwork = (typeof NETWORKS)[number];

// RuntimeForm is the fully-populated runtime config the connect/settings forms
// edit (every field required so inputs are always controlled).
export type RuntimeForm = Omit<Required<RuntimeConfig>, "network"> & {
  network: RuntimeNetwork;
};

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

// regtestDefaults target the local frontend-regtest swapdk overlay
// (regtest swapdk info). Swap gateway uses host port 10032 because
// darepod's default HTTP gateway also binds localhost:10031.
export const regtestDefaults: RuntimeForm = {
  network: "regtest",
  dataDir: "/walletdk-demo",
  arkGatewayURL: "http://127.0.0.1:7071",
  mailboxGatewayURL: "http://127.0.0.1:7071",
  walletEsploraURL: "http://127.0.0.1:8501",
  swapServerGatewayURL: "http://127.0.0.1:10032",
  swapMailboxGatewayURL: "http://127.0.0.1:10032",
  swapDatabaseFileName: "/walletdk-swaps.db",
  serverInsecure: true,
  swapServerInsecure: true,
  disableSwaps: false,
  debugLevel: "info",
};

// defaultsForNetwork returns the preset runtime form for a network selection.
export function defaultsForNetwork(network: RuntimeNetwork): RuntimeForm {
  if (network === "regtest") {
    return regtestDefaults;
  }

  return { ...signetDefaults, network };
}

// hostname extracts the host from a URL for compact display, falling back to
// the raw value when it is not a parseable URL.
export function hostname(value: string): string {
  try {
    return new URL(value).hostname;
  } catch {
    return value;
  }
}
