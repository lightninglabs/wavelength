import { AuthHeader } from "../../components/layout/AuthHeader";
import { AuthLayout } from "../../components/layout/AuthLayout";
import { Card } from "../../components/ui/Card";
import { Spinner } from "../../components/ui/Spinner";

// LoadingScreen is the boot/transition splash shown while the WASM runtime
// downloads and instantiates, and during start/stop transitions. It carries no
// action - the wallet phase advances it automatically.
export function LoadingScreen({
  network,
  title,
  sub,
  version,
  commit,
}: {
  network: string;
  title: string;
  sub: string;
  version?: string;
  commit?: string;
}) {
  return (
    <AuthLayout network={network}>
      <AuthHeader title={title} sub={sub} />
      <Card className="p-6">
        <div className="flex items-center gap-3">
          <Spinner />
          <span className="text-sm font-medium text-fg">{sub}</span>
        </div>
        <div
          className="mt-4 h-2 w-full overflow-hidden rounded-full bg-border"
        >
          <div className="h-full w-1/3 animate-pulse rounded-full bg-accent" />
        </div>
        {version || commit ? (
          <div
            className="mt-3 flex items-center justify-between font-mono text-xs
              tabular-nums text-faint"
          >
            <span>{version ? `v${version}` : ""}</span>
            <span>{commit ? `commit ${commit}` : ""}</span>
          </div>
        ) : null}
      </Card>
    </AuthLayout>
  );
}
