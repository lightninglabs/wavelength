import { useState } from "react";
import { Fingerprint, KeyRound } from "lucide-react";
import { AuthHeader } from "../../components/layout/AuthHeader";
import { AuthLayout } from "../../components/layout/AuthLayout";
import { Field } from "../../components/ui/Field";
import { InlineError } from "../../components/ui/InlineError";
import { GhostButton, PrimaryButton } from "../../components/ui/Button";

// UnlockScreen serves the `locked` phase: a wallet exists on this device and
// must be unlocked with the password (or an enrolled passkey).
export function UnlockScreen({
  network,
  passkeySupported,
  passkeyWrapAvailable,
  onUnlock,
  onUnlockPasskey,
  busy,
  error,
  passkeyBusy,
  passkeyError,
}: {
  network: string;
  passkeySupported: boolean;
  passkeyWrapAvailable: boolean;
  onUnlock: (password: string) => void;
  onUnlockPasskey: () => void;
  busy: boolean;
  error: string;
  passkeyBusy: boolean;
  passkeyError: string;
}) {
  const [password, setPassword] = useState("");
  const anyBusy = busy || passkeyBusy;
  const passkeyAvailable = passkeySupported && passkeyWrapAvailable;

  return (
    <AuthLayout network={network}>
      <AuthHeader
        title="Unlock wallet"
        sub="Unlock with your passkey or password to sync the wallet."
      />

      {passkeyAvailable ? (
        <div className="mb-4 space-y-4">
          <GhostButton
            icon={Fingerprint}
            onClick={onUnlockPasskey}
            disabled={anyBusy}
            busy={passkeyBusy}
          >
            {passkeyBusy ? "Waiting for passkey…" : "Unlock with passkey"}
          </GhostButton>
          <div className="flex items-center gap-3">
            <span className="h-px flex-1 bg-border" />
            <span className="text-xs text-faint">or use password</span>
            <span className="h-px flex-1 bg-border" />
          </div>
        </div>
      ) : null}

      <form
        className="space-y-4"
        onSubmit={(e) => {
          e.preventDefault();
          if (!anyBusy && password.length > 0) {
            onUnlock(password);
          }
        }}
      >
        <Field
          label="Password"
          type="password"
          placeholder="••••••••••"
          value={password}
          onChange={setPassword}
        />
        <PrimaryButton
          type="submit"
          icon={KeyRound}
          busy={busy}
          disabled={anyBusy || password.length === 0}
        >
          {busy ? "Unlocking…" : "Unlock"}
        </PrimaryButton>

        {anyBusy ? (
          <p className="text-center text-xs text-muted">
            Decrypting keys and syncing — this can take a few seconds.
          </p>
        ) : null}

        <InlineError message={error || passkeyError} />
      </form>
    </AuthLayout>
  );
}
