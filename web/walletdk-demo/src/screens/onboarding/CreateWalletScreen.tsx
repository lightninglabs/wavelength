import { useState } from "react";
import { Fingerprint, KeyRound, ShieldCheck } from "lucide-react";
import { AuthHeader } from "../../components/layout/AuthHeader";
import { AuthLayout } from "../../components/layout/AuthLayout";
import { Field } from "../../components/ui/Field";
import { InlineError } from "../../components/ui/InlineError";
import { GhostButton, PrimaryButton, TextLink } from "../../components/ui/Button";
import {
  WalletMode,
  WalletTypePicker,
} from "../../components/ui/WalletTypePicker";

// CreateWalletScreen creates a fresh wallet (a passkey wallet or a password
// wallet) and surfaces the secondary "already have a wallet?" affordances
// (unlock with passkey, restore from phrase) so a returning user on this device
// is never stranded. When leadWithUnlock is true the unlock affordance is
// promoted above the create form.
export function CreateWalletScreen({
  network,
  passkeySupported,
  leadWithUnlock,
  onCreate,
  onUnlockPasskey,
  onRestore,
  busy,
  error,
  passkeyBusy,
  passkeyError,
}: {
  network: string;
  passkeySupported: boolean;
  leadWithUnlock: boolean;
  onCreate: (args: { password: string; mode: WalletMode }) => void;
  onUnlockPasskey: () => void;
  onRestore: () => void;
  busy: boolean;
  error: string;
  passkeyBusy: boolean;
  passkeyError: string;
}) {
  // Default to a passkey wallet when supported; otherwise password is the only
  // option.
  const [walletMode, setWalletMode] = useState<WalletMode>(
    passkeySupported ? "passkey" : "password",
  );
  const [password, setPassword] = useState("");
  const [confirm, setConfirm] = useState("");

  // A passkey wallet derives its DB password, so the password fields are hidden
  // and do not gate submission.
  const passkeyCreate = walletMode === "passkey";
  const passwordOk = password.length > 0 && password === confirm;
  const anyBusy = busy || passkeyBusy;
  const canSubmit = passkeyCreate ? !anyBusy : !anyBusy && passwordOk;

  // submit validates canSubmit and creates the wallet in the selected mode.
  function submit() {
    if (!canSubmit) {
      return;
    }

    onCreate({ password, mode: walletMode });
  }

  // unlockButton is the passkey unlock secondary affordance, shown either above
  // (leadWithUnlock) or below the create form.
  const unlockButton = passkeySupported ? (
    <GhostButton
      icon={Fingerprint}
      onClick={onUnlockPasskey}
      disabled={anyBusy}
      busy={passkeyBusy}
    >
      {passkeyBusy ? "Waiting for passkey…" : "Unlock with passkey"}
    </GhostButton>
  ) : null;

  // unlockHint clarifies that the passkey unlock reopens an existing wallet,
  // distinguishing it from "Create passkey wallet", which makes a new one.
  const unlockHint = (
    <p className="mt-2 text-xs leading-relaxed text-muted">
      Opens a wallet you already created with this passkey on this or another
      device. It does not make a new one.
    </p>
  );

  return (
    <AuthLayout network={network}>
      <AuthHeader
        title="Create wallet"
        sub="Keys are generated and stored on this device."
      />

      {leadWithUnlock && unlockButton ? (
        <div className="mb-6">
          {unlockButton}
          {unlockHint}
          <div className="mt-2">
            <InlineError message={passkeyError} />
          </div>
          <div className="my-5 flex items-center gap-3">
            <span className="h-px flex-1 bg-border" />
            <span className="text-xs text-faint">or create a new wallet</span>
            <span className="h-px flex-1 bg-border" />
          </div>
        </div>
      ) : null}

      {passkeySupported ? (
        <div className="mb-6">
          <div className="mb-3">
            <div className="text-sm font-semibold text-fg">
              How do you want to secure this wallet?
            </div>
            <p className="mt-1 text-xs leading-relaxed text-muted">
              Pick one — you can always restore from your recovery phrase later.
            </p>
          </div>
          <WalletTypePicker value={walletMode} onChange={setWalletMode} />
        </div>
      ) : null}

      <form
        className="space-y-4"
        onSubmit={(e) => {
          e.preventDefault();
          submit();
        }}
      >
        {!passkeyCreate ? (
          <>
            <Field
              label="Password"
              type="password"
              placeholder="••••••••••"
              value={password}
              onChange={setPassword}
            />
            <Field
              label="Confirm password"
              type="password"
              placeholder="••••••••••"
              value={confirm}
              onChange={setConfirm}
            />
          </>
        ) : null}

        <PrimaryButton
          type="submit"
          icon={passkeyCreate ? Fingerprint : KeyRound}
          disabled={!canSubmit}
        >
          {busy
            ? "Creating wallet…"
            : passkeyCreate
              ? "Create passkey wallet"
              : "Create wallet"}
        </PrimaryButton>
        <InlineError message={error} />

        <div className="flex items-center gap-2 text-xs text-faint">
          <ShieldCheck size={13} className="text-good" />
          On-device keys · nothing leaves this browser.
        </div>
      </form>

      <div className="mt-6 border-t border-border pt-5">
        {!leadWithUnlock && unlockButton ? (
          <div className="mb-5">
            <div className="mb-3 flex items-center gap-3">
              <span className="h-px flex-1 bg-border" />
              <span className="text-xs text-faint">
                already have a wallet?
              </span>
              <span className="h-px flex-1 bg-border" />
            </div>
            {unlockButton}
            {unlockHint}
            <div className="mt-2">
              <InlineError message={passkeyError} />
            </div>
          </div>
        ) : null}
        <p className="text-center text-xs text-faint">
          No passkey on this device?{" "}
          <TextLink onClick={onRestore}>Restore from recovery phrase</TextLink>
        </p>
      </div>
    </AuthLayout>
  );
}
