import { useCallback, useState } from "react";
import {
  ArrowDownLeft,
  ArrowDownToLine,
  ArrowUpRight,
  ChevronRight,
  Layers,
  Lock,
  type LucideIcon,
  ShieldCheck,
  Wallet,
  Zap,
} from "lucide-react";
import { Balance, Entry, WalletInfo } from "@lightninglabs/walletdk-core";
import { ActivityRow } from "../../components/ActivityRow";
import { PageHead } from "../../components/layout/PageHead";
import { AppTab } from "../../components/layout/nav";
import { Band } from "../../components/ui/Band";
import { PrimaryButton } from "../../components/ui/Button";
import { CopyRow } from "../../components/ui/CopyRow";
import { FauxQR } from "../../components/ui/FauxQR";
import { InlineError } from "../../components/ui/InlineError";
import { Label } from "../../components/ui/Label";
import { cn } from "../../lib/cn";
import { hasAnyValue, pendingInBalance, totalBalance } from "../../lib/balance";
import { errorMessage } from "../../lib/errors";
import { formatBtc, formatSats } from "../../lib/format";
import { Composition } from "./Composition";

// HomeScreen is the authenticated overview: the balance hero with composition,
// recent activity, quick actions and runtime status, laid out as full-bleed
// Zones bands. A zero balance swaps the funded dashboard for a board-on-chain
// CTA (the only way to fund a fresh Ark wallet).
export function HomeScreen({
  balance,
  activity,
  info,
  phaseLabel,
  onNavigate,
  onDeposit,
  depositBusy,
  depositError,
}: {
  balance: Balance | null;
  activity: Entry[];
  info: WalletInfo | null;
  phaseLabel: string;
  onNavigate: (tab: AppTab) => void;
  onDeposit: () => Promise<string>;
  depositBusy: boolean;
  depositError: string;
}) {
  // Treat the wallet as funded when it holds or is receiving any value
  // (including a pending boarding deposit) or has any activity.
  const funded = hasAnyValue(balance) || activity.length > 0;

  return (
    <div>
      <PageHead
        title="Overview"
        subtitle="Your self-custodial balance across Ark, Lightning and on-chain."
      />
      {funded ? (
        <>
          <BalanceBand balance={balance} onNavigate={onNavigate} />
          <Band>
            <Label>Balance composition</Label>
            <div className="mt-4">
              <Composition balance={balance} />
            </div>
          </Band>
          <RecentActivityBand activity={activity} onNavigate={onNavigate} />
          <RuntimeBand info={info} phaseLabel={phaseLabel} />
        </>
      ) : (
        <EmptyWallet
          info={info}
          phaseLabel={phaseLabel}
          onNavigate={onNavigate}
          onDeposit={onDeposit}
          depositBusy={depositBusy}
          depositError={depositError}
        />
      )}
    </div>
  );
}

// BalanceBand is the Home hero: total balance, derived BTC, pending flow and the
// primary Send / Receive actions.
function BalanceBand({
  balance,
  onNavigate,
}: {
  balance: Balance | null;
  onNavigate: (t: AppTab) => void;
}) {
  const total = totalBalance(balance);
  const pendingIn = pendingInBalance(balance);
  const pendingOut = balance?.PendingOutSat ?? 0;

  return (
    <Band tinted>
      <div className="flex flex-wrap items-start justify-between gap-6">
        <div>
          <div className="flex items-center gap-3">
            <Label>Total balance</Label>
            <span
              className="inline-flex items-center gap-1.5 border border-border
                px-2 py-0.5 text-xs font-medium text-muted"
            >
              <span className="h-1.5 w-1.5 rounded-full bg-good" />
              Self-custody
            </span>
          </div>
          <div className="mt-2 flex items-baseline gap-2">
            <span
              className="font-mono text-5xl font-semibold tracking-tight
                tabular-nums text-fg lg:text-6xl"
            >
              {formatSats(total)}
            </span>
            <span className="text-sm font-medium text-muted">sats</span>
          </div>
          <div className="mt-1.5 flex flex-wrap items-center gap-x-3 text-sm">
            <span className="font-mono tabular-nums text-muted">
              {formatBtc(total)} BTC
            </span>
            {pendingIn > 0 ? (
              <>
                <span className="text-faint">·</span>
                <span className="font-mono tabular-nums text-warn">
                  +{formatSats(pendingIn)} pending in
                </span>
              </>
            ) : null}
            {pendingOut > 0 ? (
              <>
                <span className="text-faint">·</span>
                <span className="font-mono tabular-nums text-muted">
                  -{formatSats(pendingOut)} pending out
                </span>
              </>
            ) : null}
          </div>
        </div>
        <div className="flex gap-3">
          <button
            type="button"
            onClick={() => onNavigate("send")}
            className="inline-flex items-center gap-2 bg-accent px-4 py-2.5
              text-sm font-semibold text-white transition-opacity
              hover:opacity-90"
          >
            <ArrowUpRight size={16} /> Send
          </button>
          <button
            type="button"
            onClick={() => onNavigate("receive")}
            className="inline-flex items-center gap-2 border border-border
              [background:var(--bg)] px-4 py-2.5 text-sm font-medium text-fg
              transition-colors hover:border-border-strong"
          >
            <ArrowDownLeft size={16} /> Receive
          </button>
        </div>
      </div>
    </Band>
  );
}

// RuntimeBand surfaces live runtime telemetry and honest self-custody cues as a
// horizontal stat strip.
function RuntimeBand({
  info,
  phaseLabel,
}: {
  info: WalletInfo | null;
  phaseLabel: string;
}) {
  const rows: Array<{
    icon: LucideIcon;
    label: string;
    value: string;
    good?: boolean;
  }> = [
    { icon: ShieldCheck, label: "Runtime", value: phaseLabel, good: true },
    { icon: Zap, label: "Network", value: info?.Network || "-" },
    {
      icon: Layers,
      label: "Block height",
      value: info?.BlockHeight ? formatSats(info.BlockHeight) : "-",
    },
    { icon: Wallet, label: "Wallet", value: info?.WalletType || "-" },
    { icon: Lock, label: "Keys", value: "On this device", good: true },
  ];

  return (
    <Band>
      <Label>Runtime &amp; security</Label>
      <div className="mt-4 flex flex-wrap divide-border sm:divide-x">
        {rows.map((r) => (
          <div key={r.label} className="flex-1 px-0 sm:px-5 sm:first:pl-0">
            <div className="flex items-center gap-1.5 text-xs text-muted">
              <r.icon size={13} className={r.good ? "text-good" : "text-muted"} />
              {r.label}
            </div>
            <div
              className={cn(
                "mt-1 font-mono text-sm tabular-nums",
                r.good ? "text-good" : "text-fg",
              )}
            >
              {r.value}
            </div>
          </div>
        ))}
      </div>
    </Band>
  );
}

// EmptyWallet is the zero-balance state. It boards on-chain funds, so it fetches
// a boarding address up front and shows it inline. A Lightning top-up routes to
// the receive screen, where the user must choose an amount first.
function EmptyWallet({
  info,
  phaseLabel,
  onNavigate,
  onDeposit,
  depositBusy,
  depositError,
}: {
  info: WalletInfo | null;
  phaseLabel: string;
  onNavigate: (t: AppTab) => void;
  onDeposit: () => Promise<string>;
  depositBusy: boolean;
  depositError: string;
}) {
  const [address, setAddress] = useState("");
  const [localError, setLocalError] = useState("");

  const fetchAddress = useCallback(async () => {
    setLocalError("");
    try {
      setAddress(await onDeposit());
    } catch (err) {
      setLocalError(errorMessage(err));
    }
  }, [onDeposit]);

  const steps = [
    "Send on-chain Bitcoin to your boarding address.",
    "After 1 confirmation it boards into Ark as VTXO.",
    "Spend instantly over Ark and Lightning.",
  ];
  const error = localError || depositError;

  return (
    <>
      <Band tinted>
        <div className="mx-auto flex max-w-md flex-col items-center text-center">
          <div className="flex h-14 w-14 items-center justify-center bg-accent-soft">
            <ArrowDownToLine size={26} className="text-accent" />
          </div>
          <h2 className="mt-5 text-xl font-semibold text-fg">
            Fund your wallet
          </h2>
          <p className="mt-2 text-sm leading-relaxed text-muted">
            Your balance is empty. Send Bitcoin to your boarding address to start
            using Ark and Lightning. Funds become spendable as VTXO after one
            confirmation.
          </p>

          <div className="mt-6 w-full max-w-sm">
            {address ? (
              <div className="flex flex-col items-center gap-4">
                <div className="bg-white p-4">
                  <FauxQR
                    seed={address}
                    size={23}
                    color="#0a0a0b"
                    className="h-40 w-40"
                  />
                </div>
                <span className="text-xs text-faint">
                  This is not a real QR code
                </span>
                <div className="w-full text-left">
                  <CopyRow label="Boarding address" value={address} />
                </div>
              </div>
            ) : (
              <>
                <PrimaryButton
                  icon={ArrowDownToLine}
                  onClick={() => void fetchAddress()}
                  busy={depositBusy}
                >
                  {depositBusy ? "Generating…" : "Get a boarding address"}
                </PrimaryButton>
                <div className="mt-3">
                  <InlineError message={error} />
                </div>
              </>
            )}
          </div>

          <button
            type="button"
            onClick={() => onNavigate("receive")}
            className="mt-4 text-xs font-medium text-muted hover:text-fg"
          >
            or create a Lightning invoice
          </button>
        </div>
      </Band>

      <Band>
        <Label>How boarding works</Label>
        <ol className="mt-4 grid gap-4 sm:grid-cols-3">
          {steps.map((step, i) => (
            <li key={step} className="flex gap-3">
              <span
                className="flex h-6 w-6 shrink-0 items-center justify-center
                  bg-accent-soft text-[11px] font-bold text-accent"
              >
                {i + 1}
              </span>
              <span className="text-sm text-muted">{step}</span>
            </li>
          ))}
        </ol>
      </Band>

      <RuntimeBand info={info} phaseLabel={phaseLabel} />
    </>
  );
}

// RecentActivityBand lists the latest entries with a link to full history.
function RecentActivityBand({
  activity,
  onNavigate,
}: {
  activity: Entry[];
  onNavigate: (t: AppTab) => void;
}) {
  return (
    <Band tinted>
      <div className="flex items-center justify-between">
        <Label>Recent activity</Label>
        <button
          type="button"
          onClick={() => onNavigate("activity")}
          className="inline-flex items-center gap-1 text-xs font-medium text-accent"
        >
          View all <ChevronRight size={13} />
        </button>
      </div>
      {activity.length === 0 ? (
        <div className="mt-2 border-t border-border py-8 text-center text-sm text-muted">
          No activity yet.
        </div>
      ) : (
        <div className="mt-2 divide-y divide-border border-t border-border">
          {activity.slice(0, 4).map((entry) => (
            <ActivityRow key={entry.ID} entry={entry} />
          ))}
        </div>
      )}
    </Band>
  );
}
