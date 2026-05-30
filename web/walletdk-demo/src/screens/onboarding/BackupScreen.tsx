import { Check, TriangleAlert } from "lucide-react";
import { AuthHeader } from "../../components/layout/AuthHeader";
import { AuthLayout } from "../../components/layout/AuthLayout";
import { CopyButton } from "../../components/ui/CopyButton";
import { PrimaryButton } from "../../components/ui/Button";

// BackupScreen serves a freshly created wallet (phase ready, backup not yet
// acknowledged): it presents the generated recovery phrase once before the
// dashboard becomes reachable.
export function BackupScreen({
  network,
  mnemonic,
  onAcknowledge,
  busy,
}: {
  network: string;
  mnemonic: string[];
  onAcknowledge: () => void;
  busy: boolean;
}) {
  return (
    <AuthLayout network={network} wide>
      <AuthHeader
        title="Recovery phrase"
        sub={`Write down these ${mnemonic.length} words in order and store them offline.`}
      />
      <div
        className="mb-5 flex items-start gap-2 border border-warn/30
          bg-warn/10 p-3 text-xs text-warn"
      >
        <TriangleAlert size={14} className="mt-0.5 shrink-0" />
        Anyone with this phrase can spend your funds. Never share it.
      </div>
      <div className="grid grid-cols-2 gap-2 sm:grid-cols-3">
        {mnemonic.map((word, i) => (
          <div
            key={`${i}-${word}`}
            className="flex items-center gap-2 border border-border bg-well
              px-3 py-2"
          >
            <span className="font-mono text-xs tabular-nums text-faint">
              {String(i + 1).padStart(2, "0")}
            </span>
            <span className="text-sm font-medium text-fg">{word}</span>
          </div>
        ))}
      </div>
      <div className="mt-5 flex flex-col gap-3">
        <div className="flex justify-center">
          <CopyButton value={mnemonic.join(" ")} label="Copy phrase" />
        </div>
        <PrimaryButton icon={Check} onClick={onAcknowledge} disabled={busy}>
          I saved it
        </PrimaryButton>
      </div>
    </AuthLayout>
  );
}
