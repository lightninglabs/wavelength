import { useState } from "react";
import { Check, ShieldCheck, Zap } from "lucide-react";
import { SendRequest, SendResult } from "@lightninglabs/walletdk-core";
import { PageHead } from "../../components/layout/PageHead";
import { AppTab } from "../../components/layout/nav";
import { Band } from "../../components/ui/Band";
import { GhostButton, PrimaryButton } from "../../components/ui/Button";
import { CopyRow } from "../../components/ui/CopyRow";
import { Field } from "../../components/ui/Field";
import { InlineError } from "../../components/ui/InlineError";
import { Label } from "../../components/ui/Label";
import { SummaryRow } from "../../components/ui/SummaryRow";
import { errorMessage } from "../../lib/errors";
import { formatSats, shortKey } from "../../lib/format";

// isInvoice reports whether a destination string looks like a BOLT-11 invoice
// (versus an on-chain address).
function isInvoice(dest: string): boolean {
  return /^ln/i.test(dest.trim());
}

// SendScreen pays a BOLT-11 invoice or on-chain address, with a live review
// summary and a settled-payment confirmation showing the returned payment hash.
export function SendScreen({
  onNavigate,
  onSend,
  busy,
  error,
}: {
  onNavigate: (tab: AppTab) => void;
  onSend: (req: SendRequest) => Promise<SendResult>;
  busy: boolean;
  error: string;
}) {
  const [dest, setDest] = useState("");
  const [amount, setAmount] = useState("");
  const [maxFee, setMaxFee] = useState("0");
  const [note, setNote] = useState("");
  const [hash, setHash] = useState("");
  const [sentAmount, setSentAmount] = useState(0);
  const [localError, setLocalError] = useState("");

  async function pay() {
    setLocalError("");
    const trimmed = dest.trim();
    const req: SendRequest = isInvoice(trimmed)
      ? { invoice: trimmed }
      : { onchainAddress: trimmed };
    if (amount) {
      req.amountSat = Number(amount) || 0;
    }
    if (maxFee) {
      req.maxFeeSat = Number(maxFee) || 0;
    }
    if (note) {
      req.note = note;
    }

    try {
      const result = await onSend(req);
      setHash(result.PaymentHash || result.Entry?.ID || "");
      // Prefer the settled amount from the result (an amountless invoice has no
      // user-entered amount), falling back to the Entry then the typed value.
      setSentAmount(
        result.ActualAmountSat ||
          Math.abs(result.Entry?.AmountSat ?? 0) ||
          Number(amount) ||
          0,
      );
    } catch (err) {
      setLocalError(errorMessage(err));
    }
  }

  function reset() {
    setHash("");
    setDest("");
    setAmount("");
    setNote("");
  }

  if (hash) {
    return (
      <div>
        <PageHead
          title="Payment sent"
          subtitle="Submitted to the network"
          onBack={() => onNavigate("home")}
        />
        <Band tinted>
          <div className="mx-auto flex max-w-md flex-col items-center text-center">
            <div className="flex h-12 w-12 items-center justify-center bg-good/15">
              <Check size={22} className="text-good" />
            </div>
            <div className="mt-4 font-mono text-3xl font-semibold tabular-nums text-fg">
              {sentAmount > 0 ? formatSats(sentAmount) : "-"}
              <span className="ml-1.5 text-sm font-medium text-muted">sats</span>
            </div>
            <div className="mt-5 w-full text-left">
              <CopyRow label="Payment hash" value={hash} />
            </div>
            <div className="mt-6 grid w-full grid-cols-2 gap-3">
              <GhostButton onClick={reset}>Send another</GhostButton>
              <PrimaryButton onClick={() => onNavigate("activity")}>
                View in activity
              </PrimaryButton>
            </div>
          </div>
        </Band>
      </div>
    );
  }

  return (
    <div>
      <PageHead
        title="Send"
        subtitle="Pay an invoice or on-chain address"
        onBack={() => onNavigate("home")}
      />
      <form
        onSubmit={(e) => {
          e.preventDefault();
          if (!busy && dest.trim()) {
            pay();
          }
        }}
      >
        <Band>
          <Label>Payment details</Label>
          <div className="mt-4 space-y-4">
            <label className="block">
              <span className="mb-2 block text-[10px] font-semibold uppercase tracking-[0.16em] text-muted">
                Invoice or address
              </span>
              <textarea
                value={dest}
                onChange={(e) => setDest(e.target.value)}
                rows={3}
                placeholder="lnbc… or tb1q…"
                className="w-full resize-none border border-border bg-well px-3
                  py-3 font-mono text-xs text-fg outline-none transition-colors
                  focus:border-border-strong"
              />
            </label>
            <div className="grid gap-4 sm:grid-cols-2">
              <Field
                label="Amount (sats)"
                placeholder="from invoice"
                inputMode="numeric"
                value={amount}
                onChange={setAmount}
                mono
              />
              <Field
                label="Max routing fee (sats)"
                inputMode="numeric"
                value={maxFee}
                onChange={setMaxFee}
                mono
              />
            </div>
            <Field
              label="Note"
              placeholder="optional · stored locally"
              value={note}
              onChange={setNote}
            />
          </div>
        </Band>

        <Band tinted>
          <Label>Review</Label>
          <div className="mt-4 space-y-3 text-sm">
            <SummaryRow
              label="Destination"
              value={dest.trim() ? shortKey(dest.trim(), 10, 8) : "-"}
              mono
            />
            <SummaryRow
              label="Amount"
              value={
                amount ? `${formatSats(Number(amount) || 0)} sats` : "Per invoice"
              }
              mono
            />
            <SummaryRow
              label="Max routing fee"
              value={`${formatSats(Number(maxFee) || 0)} sats`}
              mono
            />
          </div>
          <div className="mt-4 flex items-start gap-2 border border-border bg-well p-3 text-xs text-muted">
            <ShieldCheck size={14} className="mt-0.5 shrink-0 text-accent" />
            The final amount and payment hash are returned once the payment
            settles.
          </div>
          <div className="mt-5">
            <button
              type="submit"
              disabled={busy || dest.trim().length === 0}
              className="inline-flex items-center gap-2 bg-accent px-4 py-2.5
                text-sm font-semibold text-white transition-opacity
                hover:opacity-90 disabled:opacity-50"
            >
              <Zap size={16} /> {busy ? "Paying…" : "Confirm & pay"}
            </button>
          </div>
          <div className="mt-3">
            <InlineError message={localError || error} />
          </div>
        </Band>
      </form>
    </div>
  );
}
