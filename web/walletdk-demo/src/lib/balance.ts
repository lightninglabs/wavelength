import { Balance, Entry } from "@lightninglabs/walletdk-core";

// BucketKey identifies a balance-composition bucket.
export type BucketKey = "vtxo" | "incoming" | "outgoing";

// CompositionBucket is one slice of the balance-composition visual. Values map
// to walletdkrpc.Balance: confirmed spendable VTXOs plus pending flows.
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
  incoming: "var(--good)",
  outgoing: "var(--warn)",
};

// balanceSat returns the spendable VTXO balance (walletdkrpc confirmed_sat).
export function balanceSat(balance: Balance | null): number {
  if (!balance) {
    return 0;
  }

  return Number(balance.ConfirmedSat ?? 0);
}

// pendingInSat returns in-flight inbound balance (walletdkrpc pending_in_sat).
export function pendingInSat(balance: Balance | null): number {
  if (!balance) {
    return 0;
  }

  return Number(balance.PendingInSat ?? 0);
}

// pendingOutSat returns in-flight outbound balance (walletdkrpc pending_out_sat).
export function pendingOutSat(balance: Balance | null): number {
  if (!balance) {
    return 0;
  }

  return Number(balance.PendingOutSat ?? 0);
}

// isInboundKind reports activity rows that represent funds moving into the
// wallet while still pending.
function isInboundKind(kind: string): boolean {
  return kind === "deposit" || kind === "receive";
}

// isOutboundKind reports activity rows that represent funds leaving the wallet
// while still pending.
function isOutboundKind(kind: string): boolean {
  return kind === "send" || kind === "exit";
}

// DEFAULT_DEPOSIT_FEE_SLACK covers operator fees when activity rows omit FeeSat.
const DEFAULT_DEPOSIT_FEE_SLACK = 10_000;

// depositNetSat returns the VTXO value a boarded deposit contributes: gross
// deposit minus operator fee when FeeSat is known, otherwise gross with slack
// reserved from the allocation budget.
function depositNetSat(entry: Entry): number {
  const gross = Math.abs(entry.AmountSat ?? 0);
  const fee = Math.abs(entry.FeeSat ?? 0);

  if (fee > 0 && fee < gross) {
    return gross - fee;
  }

  return gross;
}

// depositBudgetReserve is the fee slack to hold back when matching a deposit
// whose FeeSat is not yet populated on the activity row.
function depositBudgetReserve(entry: Entry): number {
  const fee = Math.abs(entry.FeeSat ?? 0);
  if (fee > 0) {
    return 0;
  }

  return DEFAULT_DEPOSIT_FEE_SLACK;
}

// entryTimeMs returns a sortable timestamp for activity rows.
function entryTimeMs(entry: Entry): number {
  const raw = entry.CreatedAt ?? entry.UpdatedAt;
  if (!raw) {
    return 0;
  }

  const ms = Date.parse(raw);
  return Number.isNaN(ms) ? 0 : ms;
}

// depositEntries returns deposit rows oldest-first for FIFO VTXO allocation.
function depositEntries(entries: Entry[]): Entry[] {
  return entries
    .filter((e) => e.Kind === "deposit")
    .slice()
    .sort((a, b) => {
      const delta = entryTimeMs(a) - entryTimeMs(b);
      if (delta !== 0) {
        return delta;
      }

      return a.ID.localeCompare(b.ID);
    });
}

// depositSettledInVtxo reports whether a deposit's funds are already reflected
// in ConfirmedSat. Deposits are matched FIFO (oldest first) so an earlier
// boarded deposit can settle while a newer one is still in flight.
function depositSettledInVtxo(
  entry: Entry,
  balance: Balance | null,
  entries: Entry[],
): boolean {
  if (entry.Kind !== "deposit" || !balance) {
    return false;
  }

  if (entry.Status === "complete") {
    return true;
  }

  if (pendingInSat(balance) > 0) {
    return false;
  }

  let budget = balanceSat(balance);
  if (budget === 0) {
    return false;
  }

  for (const dep of depositEntries(entries)) {
    const net = depositNetSat(dep);
    const reserve = depositBudgetReserve(dep);
    const cost = net + reserve;

    if (dep.Status === "complete") {
      budget = Math.max(0, budget - net);
      continue;
    }

    const settled = budget >= cost;
    if (dep.ID === entry.ID) {
      return settled;
    }

    if (settled) {
      budget -= net;
    } else {
      return false;
    }
  }

  return false;
}

// pendingInboundFromActivity sums pending deposit/receive rows when the balance
// RPC does not surface inbound boarding yet. Stale pending deposits are
// skipped once spendable VTXO balance is live (same rule as normalizeActivity).
function pendingInboundFromActivity(
  entries: Entry[],
  balance: Balance | null,
): number {
  let total = 0;

  for (const entry of entries) {
    if (entry.Status !== "pending") {
      continue;
    }

    if (!isInboundKind(entry.Kind)) {
      continue;
    }

    if (
      entry.Kind === "deposit" &&
      depositSettledInVtxo(entry, balance, entries)
    ) {
      continue;
    }

    total += Math.abs(entry.AmountSat ?? 0);
  }

  return total;
}

// pendingOutboundFromActivity sums pending send/exit rows when the balance RPC
// does not report outbound boarding sweeps yet.
function pendingOutboundFromActivity(entries: Entry[]): number {
  let total = 0;

  for (const entry of entries) {
    if (entry.Status !== "pending") {
      continue;
    }

    if (!isOutboundKind(entry.Kind)) {
      continue;
    }

    total += Math.abs(entry.AmountSat ?? 0);
  }

  return total;
}

// effectivePendingIn prefers walletdkrpc pending_in_sat and only falls back to
// pending activity when the RPC reports zero (demo UX for ledger/RPC skew).
export function effectivePendingIn(
  balance: Balance | null,
  activity: Entry[] = [],
): number {
  const rpc = pendingInSat(balance);
  if (rpc > 0) {
    return rpc;
  }

  return pendingInboundFromActivity(activity, balance);
}

// effectivePendingOut prefers walletdkrpc pending_out_sat and only falls back
// to pending activity when the RPC reports zero.
export function effectivePendingOut(
  balance: Balance | null,
  activity: Entry[] = [],
): number {
  const rpc = pendingOutSat(balance);
  if (rpc > 0) {
    return rpc;
  }

  return pendingOutboundFromActivity(activity);
}

// compositionBuckets derives the three composition rows from walletdkrpc
// Balance, supplementing incoming/outgoing from pending activity when RPC
// buckets are empty.
export function compositionBuckets(
  balance: Balance | null,
  activity: Entry[] = [],
): CompositionBucket[] {
  const b = balance || {};

  return [
    { key: "vtxo", label: "Ark VTXO", sat: b.ConfirmedSat ?? 0 },
    {
      key: "incoming",
      label: "Incoming",
      sat: effectivePendingIn(balance, activity),
    },
    {
      key: "outgoing",
      label: "Outgoing",
      sat: effectivePendingOut(balance, activity),
    },
  ];
}

// hasAnyValue reports whether the wallet holds or is moving any funds. The
// dashboard uses this (rather than balance alone) so a pending boarding
// deposit no longer leaves Overview stuck on the zero-balance CTA.
export function hasAnyValue(
  balance: Balance | null,
  activity: Entry[] = [],
): boolean {
  if (!balance && activity.length === 0) {
    return false;
  }

  return (
    balanceSat(balance) > 0 ||
    effectivePendingIn(balance, activity) > 0 ||
    effectivePendingOut(balance, activity) > 0
  );
}

// normalizeActivityEntry adjusts SDK activity rows for demo display when the
// balance view already reflects a completed boarding flow.
export function normalizeActivityEntry(
  entry: Entry,
  balance: Balance | null,
  entries: Entry[] = [],
): Entry {
  if (
    entry.Kind !== "deposit" ||
    entry.Status !== "pending" ||
    !depositSettledInVtxo(entry, balance, entries)
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
  return entries.map((entry) =>
    normalizeActivityEntry(entry, balance, entries),
  );
}
