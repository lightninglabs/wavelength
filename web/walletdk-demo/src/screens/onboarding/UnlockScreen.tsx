import { useState } from "react";
import { Fingerprint, KeyRound, RotateCcw } from "lucide-react";
import type { WalletKind } from "@lightninglabs/walletdk-core";
import { AuthHeader } from "../../components/layout/AuthHeader";
import { AuthLayout } from "../../components/layout/AuthLayout";
import { Field } from "../../components/ui/Field";
import { InlineError } from "../../components/ui/InlineError";
import { GhostButton, PrimaryButton, TextLink } from "../../components/ui/Button";

// UnlockScreen serves the `locked` phase: a wallet exists on this device and
// must be unlocked. A passkey wallet is unlocked with a platform authenticator
// (no local wrap needed, as the seed is re-derived from the passkey), while a
// password wallet uses its password. The passkey affordance is gated only on
// PRF support, so it remains available even on a device with no local marker.
export function UnlockScreen({
  network,
  passkeySupported,
  walletKind,
  onUnlock,
  onUnlockPasskey,
  onRecover,
  busy,
  error,
  passkeyBusy,
  passkeyError,
}: {
  network: string;
  passkeySupported: boolean;
  walletKind: WalletKind | null;
  onUnlock: (password: string) => void;
  onUnlockPasskey: () => void;
  onRecover: () => void;
  busy: boolean;
  error: string;
  passkeyBusy: boolean;
  passkeyError: string;
}) {
  const [password, setPassword] = useState("");
  const anyBusy = busy || passkeyBusy;

  // The passkey option does not depend on a locally stored wrap: it is offered
  // whenever the device supports PRF and the wallet is not explicitly a password
  // wallet (an unknown marker still permits a passkey attempt).
  const showPasskey = passkeySupported && walletKind !== "password";
  const showPassword = walletKind === "password" || (!showPasskey && walletKind === null);

  // The subtitle names only the methods actually offered, so a passkey wallet
  // (which has no password) is never told it can unlock with one.
  const sub =
    showPasskey && showPassword
      ? "Unlock with your passkey or password to sync the wallet."
      : showPasskey
        ? "Unlock with your passkey to sync the wallet."
        : showPassword
          ? "Unlock with your password to sync the wallet."
          : "Unlock to sync the wallet.";

  return (
    <AuthLayout network={network}>
      <AuthHeader title="Unlock wallet" sub={sub} />

      {showPasskey ? (
        <div className="mb-4 space-y-4">
          <GhostButton
            icon={Fingerprint}
            onClick={onUnlockPasskey}
            disabled={anyBusy}
            busy={passkeyBusy}
          >
            {passkeyBusy ? "Waiting for passkey…" : "Unlock with passkey"}
          </GhostButton>
          {showPassword ? (
            <div className="flex items-center gap-3">
              <span className="h-px flex-1 bg-border" />
              <span className="text-xs text-faint">or use password</span>
              <span className="h-px flex-1 bg-border" />
            </div>
          ) : null}
        </div>
      ) : null}

      {showPassword ? (
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
              Decrypting keys and syncing. This can take a few seconds.
            </p>
          ) : null}
        </form>
      ) : null}

      <InlineError message={error || passkeyError} />

      <div className="mt-5 flex items-center justify-center gap-1.5 text-xs text-faint">
        <RotateCcw size={13} />
        <span>Lost access?</span>
        <TextLink onClick={onRecover}>Recover with phrase</TextLink>
      </div>
    </AuthLayout>
  );
}
