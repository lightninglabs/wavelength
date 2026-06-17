import { Balance, Entry } from "@lightninglabs/walletdk-core";
import { BUCKET_TONE, compositionBuckets } from "../../lib/balance";
import { formatSats, pct } from "../../lib/format";

// Composition is the balance-composition graph: a hairline segmented meter over
// a three-column grid (Ark VTXO, incoming, outgoing) sourced from walletdkrpc
// Balance with a pending-activity fallback when RPC inbound/outbound is zero.
export function Composition({
  balance,
  activity,
}: {
  balance: Balance | null;
  activity: Entry[];
}) {
  const buckets = compositionBuckets(balance, activity);
  const total = buckets.reduce((sum, b) => sum + b.sat, 0) || 1;

  return (
    <div>
      <div className="flex h-2.5 w-full overflow-hidden rounded-full bg-border">
        {buckets.map((b) =>
          b.sat > 0 ? (
            <div
              key={b.key}
              style={{
                width: `${pct(b.sat, total)}%`,
                background: BUCKET_TONE[b.key],
              }}
            />
          ) : null,
        )}
      </div>
      <div className="mt-5 grid grid-cols-3 gap-x-6 gap-y-4">
        {buckets.map((b) => (
          <div key={b.key}>
            <div className="flex items-center justify-between text-sm">
              <span className="flex items-center gap-2 text-muted">
                <span
                  className="h-2 w-2 rounded-full"
                  style={{ background: BUCKET_TONE[b.key] }}
                />
                {b.label}
              </span>
              <span className="font-mono tabular-nums text-fg">
                {formatSats(b.sat)}
              </span>
            </div>
            <div className="mt-2 h-1.5 w-full overflow-hidden rounded-full bg-border">
              <div
                className="h-full rounded-full"
                style={{
                  width: `${pct(b.sat, total)}%`,
                  background: BUCKET_TONE[b.key],
                }}
              />
            </div>
            <div className="mt-1 font-mono text-[11px] tabular-nums text-faint">
              {pct(b.sat, total).toFixed(1)}%
            </div>
          </div>
        ))}
      </div>
    </div>
  );
}
