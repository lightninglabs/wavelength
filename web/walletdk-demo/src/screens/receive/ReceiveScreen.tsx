import { useState } from "react";
import { Layers, Zap } from "lucide-react";
import { ReceiveRequest } from "@lightninglabs/walletdk-core";
import { PageHead } from "../../components/layout/PageHead";
import { AppTab } from "../../components/layout/nav";
import { Band } from "../../components/ui/Band";
import { GhostButton, PrimaryButton } from "../../components/ui/Button";
import { CopyRow } from "../../components/ui/CopyRow";
import { FauxQR } from "../../components/ui/FauxQR";
import { Field } from "../../components/ui/Field";
import { InlineError } from "../../components/ui/InlineError";
import { Label } from "../../components/ui/Label";
import { Segmented } from "../../components/ui/Segmented";
import { errorMessage } from "../../lib/errors";

type Tab = "lightning" | "onchain";

// ReceiveScreen offers a Lightning invoice (amount + memo) or an on-chain
// boarding address, each paired with a QR. Values come from the live
// receive()/deposit() calls.
export function ReceiveScreen({
  onNavigate,
  onReceive,
  onDeposit,
  receiveBusy,
  receiveError,
  depositBusy,
  depositError,
}: {
  onNavigate: (tab: AppTab) => void;
  onReceive: (req: ReceiveRequest) => Promise<string>;
  onDeposit: () => Promise<string>;
  receiveBusy: boolean;
  receiveError: string;
  depositBusy: boolean;
  depositError: string;
}) {
  const [tab, setTab] = useState<Tab>("lightning");
  const [amount, setAmount] = useState("1000");
  const [memo, setMemo] = useState("");
  const [invoice, setInvoice] = useState("");
  const [address, setAddress] = useState("");
  const [localError, setLocalError] = useState("");

  const isLn = tab === "lightning";
  // The QR result only appears once a value has been generated.
  const result = isLn ? invoice : address;

  async function createInvoice() {
    setLocalError("");
    try {
      const next = await onReceive({
        amountSat: Number(amount) || 0,
        memo: memo || undefined,
      });
      setInvoice(next);
    } catch (err) {
      setLocalError(errorMessage(err));
    }
  }

  async function getAddress() {
    setLocalError("");
    try {
      setAddress(await onDeposit());
    } catch (err) {
      setLocalError(errorMessage(err));
    }
  }

  return (
    <div>
      <PageHead
        title="Receive"
        subtitle="Share an invoice or boarding address"
        onBack={() => onNavigate("home")}
      />

      <Band>
        <Label>Method</Label>
        <div className="mt-3">
          <Segmented
            value={tab}
            onChange={(t) => setTab(t)}
            options={[
              { value: "lightning", label: "Lightning" },
              { value: "onchain", label: "On-chain" },
            ]}
          />
        </div>

        {isLn ? (
          <div className="mt-5 grid gap-4 sm:grid-cols-2">
            <Field
              label="Amount (sats)"
              inputMode="numeric"
              value={amount}
              onChange={setAmount}
              mono
            />
            <Field label="Memo" value={memo} onChange={setMemo} />
          </div>
        ) : (
          <div className="mt-5 flex items-start gap-2 border border-border bg-well p-3 text-xs text-muted">
            <Layers size={14} className="mt-0.5 shrink-0 text-accent" />
            Funds board into Ark after 1 confirmation.
          </div>
        )}

        <div className="mt-5 flex flex-wrap items-center gap-3">
          {isLn ? (
            <PrimaryButton
              icon={Zap}
              onClick={createInvoice}
              busy={receiveBusy}
              block={false}
            >
              {receiveBusy
                ? "Creating invoice…"
                : invoice
                  ? "Create another"
                  : "Create invoice"}
            </PrimaryButton>
          ) : address ? (
            <GhostButton onClick={() => onNavigate("home")} block={false}>
              Done
            </GhostButton>
          ) : (
            <PrimaryButton
              icon={Layers}
              onClick={getAddress}
              busy={depositBusy}
              block={false}
            >
              {depositBusy ? "Generating…" : "Get boarding address"}
            </PrimaryButton>
          )}
        </div>
        <div className="mt-3">
          <InlineError
            message={localError || (isLn ? receiveError : depositError)}
          />
        </div>
      </Band>

      {result ? (
        <Band tinted>
          <div className="flex flex-col items-center gap-4 text-center">
            <div className="bg-white p-4">
              <FauxQR
                seed={result}
                size={23}
                color="#0a0a0b"
                className="h-44 w-44"
              />
            </div>
            <span className="text-xs text-faint">
              This is not a real QR code
            </span>
            <div className="w-full max-w-md text-left">
              <CopyRow
                label={isLn ? "Invoice" : "Boarding address"}
                value={result}
              />
            </div>
          </div>
        </Band>
      ) : null}
    </div>
  );
}
