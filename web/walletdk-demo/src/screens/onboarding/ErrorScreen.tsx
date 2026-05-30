import { RefreshCw, TriangleAlert } from "lucide-react";
import { AuthHeader } from "../../components/layout/AuthHeader";
import { AuthLayout } from "../../components/layout/AuthLayout";
import { Card } from "../../components/ui/Card";
import { PrimaryButton } from "../../components/ui/Button";

// ErrorScreen serves the `error` phase: the runtime failed to initialise or
// start. It surfaces the message and offers a retry.
export function ErrorScreen({
  network,
  message,
  onRetry,
  busy,
}: {
  network: string;
  message: string;
  onRetry: () => void;
  busy: boolean;
}) {
  return (
    <AuthLayout network={network}>
      <AuthHeader
        title="Runtime error"
        sub="Something went wrong starting the wallet runtime."
      />
      <Card className="p-6">
        <div className="flex items-start gap-3">
          <span
            className="flex h-10 w-10 shrink-0 items-center justify-center
              border border-bad/30 bg-bad/10 text-bad"
          >
            <TriangleAlert size={18} />
          </span>
          <p className="break-words text-sm text-fg">
            {message || "Unknown error."}
          </p>
        </div>
      </Card>
      <div className="mt-5">
        <PrimaryButton icon={RefreshCw} onClick={onRetry} disabled={busy}>
          {busy ? "Retrying…" : "Try again"}
        </PrimaryButton>
      </div>
    </AuthLayout>
  );
}
