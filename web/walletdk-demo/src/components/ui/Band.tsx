import { ReactNode } from "react";
import { cn } from "../../lib/cn";

// Band is the core Zones surface: a full-bleed horizontal zone whose background
// (and a hairline top/bottom edge when tinted) separates a section, replacing
// the old rounded cards. Content is centred to the shared max width; the tint
// bleeds edge to edge.
export function Band({
  children,
  tinted,
  className,
}: {
  children: ReactNode;
  tinted?: boolean;
  className?: string;
}) {
  return (
    <div
      className={cn(
        "w-full py-8",
        tinted && "border-y border-border [background:var(--surface-alt)]",
        className,
      )}
    >
      <div className="mx-auto max-w-6xl px-4 lg:px-8">{children}</div>
    </div>
  );
}
