import { Balance } from "@lightninglabs/walletdk-core";

// BucketKey identifies a balance-composition bucket.
export type BucketKey = "vtxo" | "boarding" | "onchain" | "pending";

// CompositionBucket is one slice of the balance-composition visual. WalletDK
// exposes no price oracle or balance history, so the only honest breakdown is
// the live composition of the confirmed balance plus pending-in.
export type CompositionBucket = {
  key: BucketKey;
  label: string;
  sat: number;
};

// BUCKET_TONE maps each bucket to a CSS colour variable so the meter renders
// correctly under both light and dark palettes (the values are set in
// index.css). These are raw CSS colours because the widths are inline-styled.
export const BUCKET_TONE: Record<BucketKey, string> = {
  vtxo: "var(--accent)",
  boarding: "var(--good)",
  onchain: "var(--muted)",
  pending: "var(--warn)",
};

// compositionBuckets derives the ordered balance-composition buckets from the
// real `balance()` result. Boarding folds its confirmed + unconfirmed sats.
export function compositionBuckets(
  balance: Balance | null,
): CompositionBucket[] {
  const b = balance || {};

  return [
    { key: "vtxo", label: "Ark VTXO", sat: b.VTXOBalanceSat ?? 0 },
    {
      key: "boarding",
      label: "Boarding",
      sat: (b.BoardingConfirmedSat ?? 0) + (b.BoardingUnconfirmedSat ?? 0),
    },
    { key: "onchain", label: "On-chain", sat: b.OnchainWalletConfirmedSat ?? 0 },
    { key: "pending", label: "Pending in", sat: b.PendingInSat ?? 0 },
  ];
}

// totalBalance returns the spendable confirmed total, preferring the SDK's
// TotalConfirmedSat and falling back to ConfirmedSat.
export function totalBalance(balance: Balance | null): number {
  if (!balance) {
    return 0;
  }

  return Number(balance.TotalConfirmedSat ?? balance.ConfirmedSat ?? 0);
}

// pendingInBalance returns the in-flight inbound total: an unconfirmed
// boarding deposit (the common case for a fresh wallet) plus any other
// pending-in the SDK reports.
export function pendingInBalance(balance: Balance | null): number {
  if (!balance) {
    return 0;
  }

  return (
    Number(balance.BoardingUnconfirmedSat ?? 0) +
    Number(balance.PendingInSat ?? 0)
  );
}

// hasAnyValue reports whether the wallet holds or is receiving any funds
// across every category. The dashboard uses this (rather than just the
// confirmed total) so a pending boarding deposit no longer leaves the
// Overview stuck on the zero-balance "fund your wallet" state.
export function hasAnyValue(balance: Balance | null): boolean {
  if (!balance) {
    return false;
  }

  return (
    totalBalance(balance) > 0 ||
    Number(balance.VTXOBalanceSat ?? 0) > 0 ||
    Number(balance.BoardingConfirmedSat ?? 0) > 0 ||
    Number(balance.BoardingUnconfirmedSat ?? 0) > 0 ||
    Number(balance.OnchainWalletConfirmedSat ?? 0) > 0 ||
    pendingInBalance(balance) > 0
  );
}
