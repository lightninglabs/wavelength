import {
  Activity,
  ArrowDownLeft,
  ArrowUpRight,
  Layers,
  LogOut,
  type LucideIcon,
} from "lucide-react";
import { Entry } from "@lightninglabs/walletdk-core";
import { cn } from "../lib/cn";
import { formatSats, formatTimestamp, shortKey } from "../lib/format";

const KIND_ICON: Record<string, LucideIcon> = {
  receive: ArrowDownLeft,
  send: ArrowUpRight,
  deposit: Layers,
  exit: LogOut,
};

const KIND_LABEL: Record<string, string> = {
  receive: "Received",
  send: "Sent",
  deposit: "Boarding deposit",
  exit: "Unilateral exit",
};

const STATUS_CLASS: Record<string, string> = {
  complete: "border-border text-good",
  pending: "border-warn/40 bg-warn/10 text-warn",
  failed: "border-bad/40 bg-bad/10 text-bad",
};

// ActivityRow renders a single dense transaction line from an SDK Entry. The
// counterparty is a bare string (pubkey / address / invoice), so the local note
// is the title and the raw counterparty is shown monospace beneath - never a
// name (there is no contacts API).
export function ActivityRow({ entry }: { entry: Entry }) {
  const Icon = KIND_ICON[entry.Kind] ?? Activity;
  const incoming = entry.Kind === "receive" || entry.Kind === "deposit";
  const failed = entry.Status === "failed";
  const sign = incoming ? "+" : "-";
  const title = entry.Note || KIND_LABEL[entry.Kind] || entry.Kind;
  const time = formatTimestamp(entry.CreatedAt);

  return (
    <div
      data-testid="activity-row"
      className="flex items-center gap-3 py-3"
    >
      <div
        className="flex h-9 w-9 shrink-0 items-center justify-center border
          border-border"
      >
        <Icon size={15} className={incoming ? "text-good" : "text-fg"} />
      </div>
      <div className="min-w-0 flex-1">
        <div className="truncate text-sm font-medium text-fg">{title}</div>
        {entry.Counterparty ? (
          <div className="truncate font-mono text-xs text-muted">
            {shortKey(entry.Counterparty, 10, 6)}
            {time ? ` · ${time}` : ""}
          </div>
        ) : time ? (
          <div className="truncate font-mono text-xs text-muted">{time}</div>
        ) : null}
        {failed && entry.FailureReason ? (
          <div className="truncate text-xs text-bad">{entry.FailureReason}</div>
        ) : null}
      </div>
      <div className="hidden sm:block">
        <span
          className={cn(
            `border px-2 py-0.5 text-[10px] font-medium uppercase
            tracking-wide`,
            STATUS_CLASS[entry.Status] ?? "border-border text-muted",
          )}
        >
          {entry.Status}
        </span>
      </div>
      <div className="text-right">
        <div
          className={cn(
            "text-sm font-medium tabular-nums font-mono",
            failed ? "text-faint" : incoming ? "text-good" : "text-fg",
          )}
        >
          {sign}
          {formatSats(Math.abs(entry.AmountSat))}
        </div>
        {entry.FeeSat && entry.FeeSat > 0 ? (
          <div className="font-mono text-[11px] tabular-nums text-faint">
            fee {formatSats(entry.FeeSat)}
          </div>
        ) : null}
      </div>
    </div>
  );
}
