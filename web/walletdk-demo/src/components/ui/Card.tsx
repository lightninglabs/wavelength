import { CSSProperties, ReactNode } from "react";
import { cn } from "../../lib/cn";

// Card is a flat, square-cornered surface with a hairline border. The Zones
// redesign separates sections with full-bleed Bands instead; Card remains for
// the few places that need a self-contained bordered panel.
export function Card({
  children,
  className,
  style,
}: {
  children: ReactNode;
  className?: string;
  style?: CSSProperties;
}) {
  return (
    <div
      className={cn("border border-border bg-surface", className)}
      style={style}
    >
      {children}
    </div>
  );
}
