import { Balance, Entry } from "@lightninglabs/walletdk-core";

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

// spentBoardingOnchainOverlap reports whether onchain_wallet_confirmed_sat
// likely still credits a boarding deposit that already landed as a VTXO.
// btcwallet keeps the boarding UTXO in its balance after the round spends it;
// the excess is usually just the operator fee taken at seal time.
function spentBoardingOnchainOverlap(balance: Balance): boolean {
  const vtxo = Number(balance.VTXOBalanceSat ?? 0);
  const boarding =
    Number(balance.BoardingConfirmedSat ?? 0) +
    Number(balance.BoardingUnconfirmedSat ?? 0);
  const onchain = Number(balance.OnchainWalletConfirmedSat ?? 0);

  if (vtxo <= 0 || boarding > 0 || onchain <= 0 || onchain < vtxo) {
    return false;
  }

  const feeFraction = (onchain - vtxo) / onchain;

  return feeFraction <= 0.02;
}

// onchainWalletBalanceSat returns the on-chain bucket for composition. The
// daemon exposes btcwallet's confirmed balance separately from Ark totals;
// after boarding, that field can still show the original deposit even though
// the UTXO was spent into a VTXO. Suppress that overlap so the chart matches
// TotalConfirmedSat instead of double-counting the deposit.
export function onchainWalletBalanceSat(balance: Balance | null): number {
  if (!balance) {
    return 0;
  }

  const onchain = Number(balance.OnchainWalletConfirmedSat ?? 0);
  if (onchain <= 0 || spentBoardingOnchainOverlap(balance)) {
    return 0;
  }

  return onchain;
}

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
    { key: "onchain", label: "On-chain", sat: onchainWalletBalanceSat(b) },
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
    onchainWalletBalanceSat(balance) > 0 ||
    pendingInBalance(balance) > 0
  );
}

// boardingDepositComplete reports whether a pending deposit activity row should
// be treated as complete: the ledger keeps it "boarding" until the daemon marks
// the round confirmed, but a live VTXO with no boarding balance means the funds
// already arrived in Ark.
function boardingDepositComplete(balance: Balance | null): boolean {
  if (!balance) {
    return false;
  }

  const vtxo = Number(balance.VTXOBalanceSat ?? 0);
  const boarding =
    Number(balance.BoardingConfirmedSat ?? 0) +
    Number(balance.BoardingUnconfirmedSat ?? 0);

  return vtxo > 0 && boarding === 0;
}

// normalizeActivityEntry adjusts SDK activity rows for demo display when the
// balance view already reflects a completed boarding flow.
export function normalizeActivityEntry(
  entry: Entry,
  balance: Balance | null,
): Entry {
  if (
    entry.Kind !== "deposit" ||
    entry.Status !== "pending" ||
    !boardingDepositComplete(balance)
  ) {
    return entry;
  }

  return { ...entry, Status: "complete" };
}

// normalizeActivity maps normalizeActivityEntry across a list.
export function normalizeActivity(
  entries: Entry[],
  balance: Balance | null,
): Entry[] {
  return entries.map((entry) => normalizeActivityEntry(entry, balance));
}
