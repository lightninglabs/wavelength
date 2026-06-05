import type { WalletKind } from "@lightninglabs/walletdk-core";

const prefix = "walletdk:wallet-kind:";

// readWalletKind returns the stored wallet kind for a data dir, or null.
export function readWalletKind(dataDir: string): WalletKind | null {
  const v = localStorage.getItem(prefix + dataDir);

  return v === "passkey" || v === "password" ? v : null;
}

// writeWalletKind records the wallet kind for a data dir.
export function writeWalletKind(dataDir: string, kind: WalletKind): void {
  localStorage.setItem(prefix + dataDir, kind);
}

const credPrefix = "walletdk:passkey-cred:";

// readPasskeyCredentialId returns the stored passkey credential id for a data
// dir, or null when none has been recorded yet (e.g. a fresh device).
export function readPasskeyCredentialId(dataDir: string): string | null {
  return localStorage.getItem(credPrefix + dataDir);
}

// writePasskeyCredentialId records the credential id used to open a passkey
// wallet so later unlocks on this device can be scoped to it (no OS chooser).
export function writePasskeyCredentialId(dataDir: string, id: string): void {
  localStorage.setItem(credPrefix + dataDir, id);
}
