import { Field } from "./ui/Field";
import { ToggleRow } from "./ui/ToggleRow";
import { RuntimeFieldSetter, RuntimeForm } from "../lib/runtime-config";

// GatewayFields renders the runtime gateway endpoints and security toggles
// bound to a RuntimeForm. `disabled` makes them display-only, used by the
// Settings advanced section, where the running config cannot be edited.
export function GatewayFields({
  form,
  onField,
  disabled = false,
}: {
  form: RuntimeForm;
  onField: RuntimeFieldSetter;
  disabled?: boolean;
}) {
  return (
    <div className="space-y-4">
      <Field
        label="Ark gateway URL"
        value={form.arkGatewayURL}
        onChange={(v) => onField("arkGatewayURL", v)}
        mono
        disabled={disabled}
      />
      <Field
        label="Mailbox gateway URL"
        value={form.mailboxGatewayURL}
        onChange={(v) => onField("mailboxGatewayURL", v)}
        mono
        disabled={disabled}
      />
      <Field
        label="Wallet Esplora URL"
        value={form.walletEsploraURL}
        onChange={(v) => onField("walletEsploraURL", v)}
        mono
        disabled={disabled}
      />
      <Field
        label="Swap server gateway URL"
        value={form.swapServerGatewayURL}
        onChange={(v) => onField("swapServerGatewayURL", v)}
        mono
        disabled={disabled}
      />
      <Field
        label="Swap mailbox gateway URL"
        value={form.swapMailboxGatewayURL}
        onChange={(v) => onField("swapMailboxGatewayURL", v)}
        mono
        disabled={disabled}
      />
      <div className="grid grid-cols-2 gap-3">
        <Field
          label="Data directory"
          value={form.dataDir}
          onChange={(v) => onField("dataDir", v)}
          mono
          disabled={disabled}
        />
        <Field
          label="Swap database file"
          value={form.swapDatabaseFileName}
          onChange={(v) => onField("swapDatabaseFileName", v)}
          mono
          disabled={disabled}
        />
      </div>
      <Field
        label="Debug level"
        value={form.debugLevel}
        onChange={(v) => onField("debugLevel", v)}
        disabled={disabled}
      />
      <ToggleRow
        title="Allow insecure transport"
        subtitle="Permit non-TLS Ark gateway connections"
        on={form.serverInsecure}
        onChange={(v) => onField("serverInsecure", v)}
        disabled={disabled}
      />
      <ToggleRow
        title="Allow insecure swap transport"
        subtitle="Permit non-TLS swap gateway connections"
        on={form.swapServerInsecure}
        onChange={(v) => onField("swapServerInsecure", v)}
        disabled={disabled}
      />
      <ToggleRow
        title="Disable swaps"
        subtitle="Run without the submarine-swap server"
        on={form.disableSwaps}
        onChange={(v) => onField("disableSwaps", v)}
        disabled={disabled}
      />
    </div>
  );
}
