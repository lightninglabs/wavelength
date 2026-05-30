import { useState } from "react";
import { ChevronDown, Power } from "lucide-react";
import { AuthHeader } from "../../components/layout/AuthHeader";
import { AuthLayout } from "../../components/layout/AuthLayout";
import { Field } from "../../components/ui/Field";
import { InlineError } from "../../components/ui/InlineError";
import { PrimaryButton } from "../../components/ui/Button";
import { Segmented } from "../../components/ui/Segmented";
import { ToggleRow } from "../../components/ui/ToggleRow";
import { cn } from "../../lib/cn";
import {
  NETWORKS,
  RuntimeFieldSetter,
  RuntimeForm,
} from "../../lib/runtime-config";

// ConnectScreen is the "Start runtime" screen (phase runtimeReady): pick a
// network, set the gateway endpoints (basic + advanced) and start. The runtime
// then decides whether the next step is create, unlock or sync.
export function ConnectScreen({
  form,
  onField,
  onStart,
  busy,
  error,
}: {
  form: RuntimeForm;
  onField: RuntimeFieldSetter;
  onStart: () => void;
  busy: boolean;
  error: string;
}) {
  const [advanced, setAdvanced] = useState(false);

  return (
    <AuthLayout network={form.network} wide>
      <AuthHeader
        title="Start runtime"
        sub="Choose a network and the gateways the runtime should connect to."
      />
      <form
        className="space-y-4"
        onSubmit={(e) => {
          e.preventDefault();
          onStart();
        }}
      >
        <div>
          <span className="mb-2 block text-[10px] font-semibold uppercase tracking-[0.16em] text-muted">
            Network
          </span>
          <Segmented
            value={form.network}
            onChange={(v) => onField("network", v)}
            options={NETWORKS.map((n) => ({ value: n, label: n }))}
          />
        </div>

        <div className="border border-border">
          <button
            type="button"
            onClick={() => setAdvanced((v) => !v)}
            className="flex w-full items-center justify-between px-4 py-3"
          >
            <span className="text-sm font-medium text-fg">
              Advanced endpoints
            </span>
            <ChevronDown
              size={16}
              className={cn(
                "text-muted transition-transform",
                advanced && "rotate-180",
              )}
            />
          </button>
          {advanced ? (
            <div className="space-y-4 border-t border-border px-4 pb-4 pt-4">
              <Field
                label="Ark gateway URL"
                value={form.arkGatewayURL}
                onChange={(v) => onField("arkGatewayURL", v)}
                mono
              />
              <Field
                label="Wallet Esplora URL"
                value={form.walletEsploraURL}
                onChange={(v) => onField("walletEsploraURL", v)}
                mono
              />
              <Field
                label="Mailbox gateway URL"
                value={form.mailboxGatewayURL}
                onChange={(v) => onField("mailboxGatewayURL", v)}
                mono
              />
              <Field
                label="Swap server gateway URL"
                value={form.swapServerGatewayURL}
                onChange={(v) => onField("swapServerGatewayURL", v)}
                mono
              />
              <Field
                label="Swap mailbox gateway URL"
                value={form.swapMailboxGatewayURL}
                onChange={(v) => onField("swapMailboxGatewayURL", v)}
                mono
              />
              <div className="grid grid-cols-2 gap-3">
                <Field
                  label="Data directory"
                  value={form.dataDir}
                  onChange={(v) => onField("dataDir", v)}
                  mono
                />
                <Field
                  label="Swap database file"
                  value={form.swapDatabaseFileName}
                  onChange={(v) => onField("swapDatabaseFileName", v)}
                  mono
                />
              </div>
              <Field
                label="Debug level"
                value={form.debugLevel}
                onChange={(v) => onField("debugLevel", v)}
              />
              <ToggleRow
                title="Allow insecure transport"
                subtitle="Permit non-TLS Ark gateway connections"
                on={form.serverInsecure}
                onChange={(v) => onField("serverInsecure", v)}
              />
              <ToggleRow
                title="Allow insecure swap transport"
                subtitle="Permit non-TLS swap gateway connections"
                on={form.swapServerInsecure}
                onChange={(v) => onField("swapServerInsecure", v)}
              />
              <ToggleRow
                title="Disable swaps"
                subtitle="Run without the submarine-swap server"
                on={form.disableSwaps}
                onChange={(v) => onField("disableSwaps", v)}
              />
            </div>
          ) : null}
        </div>

        <PrimaryButton type="submit" icon={Power} disabled={busy}>
          {busy ? "Starting runtime…" : "Start runtime"}
        </PrimaryButton>
        <InlineError message={error} />
      </form>
    </AuthLayout>
  );
}
