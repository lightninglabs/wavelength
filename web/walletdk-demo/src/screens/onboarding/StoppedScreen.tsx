import { Power } from "lucide-react";
import { AuthHeader } from "../../components/layout/AuthHeader";
import { AuthLayout } from "../../components/layout/AuthLayout";
import { Card } from "../../components/ui/Card";
import { PrimaryButton } from "../../components/ui/Button";

// StoppedScreen serves the `stopped` phase: the runtime was torn down and its
// in-memory keys cleared. Starting again re-runs the connect flow.
export function StoppedScreen({
  network,
  onStart,
  busy,
  blockHeight,
  version,
}: {
  network: string;
  onStart: () => void;
  busy: boolean;
  blockHeight?: number;
  version?: string;
}) {
  return (
    <AuthLayout network={network}>
      <AuthHeader
        title="Runtime stopped"
        sub={`The runtime was torn down. Start it again to reconnect to the ${network} gateways.`}
      />
      <Card className="p-6">
        <div className="flex items-center gap-3">
          <span
            className="flex h-11 w-11 items-center justify-center border
              border-border bg-surface-alt"
          >
            <Power size={20} className="text-muted" />
          </span>
          <div>
            <div className="text-sm font-medium text-fg">
              In-memory keys cleared
            </div>
            {blockHeight || version ? (
              <div className="font-mono text-xs tabular-nums text-faint">
                {blockHeight ? `last block ${blockHeight}` : ""}
                {blockHeight && version ? " · " : ""}
                {version ? `v${version}` : ""}
              </div>
            ) : null}
          </div>
        </div>
      </Card>
      <div className="mt-5">
        <PrimaryButton icon={Power} onClick={onStart} disabled={busy}>
          {busy ? "Starting runtime…" : "Start runtime"}
        </PrimaryButton>
      </div>
      <p className="mt-3 text-center text-xs text-faint">
        You will need your password or passkey to unlock again.
      </p>
    </AuthLayout>
  );
}
