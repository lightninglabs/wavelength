import { ReactNode } from "react";
import { ArrowLeft } from "lucide-react";

// PageHead is the full-bleed header bar atop authenticated sub-pages: a square
// back control, the title block and an optional trailing slot, centred to the
// shared content width so it lines up with the Bands below it.
export function PageHead({
  title,
  subtitle,
  onBack,
  trailing,
}: {
  title: string;
  subtitle: string;
  onBack?: () => void;
  trailing?: ReactNode;
}) {
  return (
    <div className="w-full pt-8 pb-2">
      <div className="mx-auto flex max-w-6xl items-center gap-3 px-4 lg:px-8">
        {onBack ? (
          <button
            type="button"
            onClick={onBack}
            aria-label="Back"
            className="flex h-8 w-8 items-center justify-center border
              border-border text-muted transition-colors hover:text-fg"
          >
            <ArrowLeft size={15} />
          </button>
        ) : null}
        <div>
          <h1 className="text-lg font-semibold text-fg">{title}</h1>
          <p className="text-sm text-muted">{subtitle}</p>
        </div>
        {trailing ? <div className="ml-auto">{trailing}</div> : null}
      </div>
    </div>
  );
}
