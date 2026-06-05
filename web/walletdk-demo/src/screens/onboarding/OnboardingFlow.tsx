import { useState } from "react";
import { CreateWalletScreen } from "./CreateWalletScreen";
import { RestoreWalletScreen } from "./RestoreWalletScreen";
import {
  readPasskeyCredentialId,
  readWalletKind,
} from "../../lib/walletKind";

type Step = "create" | "restore";

// OnboardingFlow serves the needsWallet phase (runtime started, no local
// wallet). It routes between creating a wallet and restoring one from a phrase,
// deriving the default from local state instead of asking the user to pick from
// a menu.
export function OnboardingFlow({
  network,
  dataDir,
  passkeySupported,
  onCreate,
  onRestore,
  onUnlockPasskey,
  busy,
  error,
  passkeyBusy,
  passkeyError,
}: {
  network: string;
  dataDir: string;
  passkeySupported: boolean;
  onCreate: (args: { password: string; mode: "passkey" | "password" }) => void;
  onRestore: (args: {
    password: string;
    mnemonic: string[];
    passphrase: string;
  }) => void;
  onUnlockPasskey: () => void;
  busy: boolean;
  error: string;
  passkeyBusy: boolean;
  passkeyError: string;
}) {
  const [step, setStep] = useState<Step>("create");

  // A recorded passkey credential id (or a passkey wallet-kind marker) for this
  // data dir means a passkey wallet lived here before (e.g. OPFS was cleared but
  // localStorage survived); lead with unlock so the user re-opens it rather than
  // minting a second wallet.
  const leadWithUnlock =
    passkeySupported &&
    (readPasskeyCredentialId(dataDir) !== null ||
      readWalletKind(dataDir) === "passkey");

  if (step === "restore") {
    return (
      <RestoreWalletScreen
        network={network}
        onRestore={onRestore}
        onBack={() => setStep("create")}
        busy={busy}
        error={error}
      />
    );
  }

  return (
    <CreateWalletScreen
      network={network}
      passkeySupported={passkeySupported}
      leadWithUnlock={leadWithUnlock}
      onCreate={onCreate}
      onUnlockPasskey={onUnlockPasskey}
      onRestore={() => setStep("restore")}
      busy={busy}
      error={error}
      passkeyBusy={passkeyBusy}
      passkeyError={passkeyError}
    />
  );
}
