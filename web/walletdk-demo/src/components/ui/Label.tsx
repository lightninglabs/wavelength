import { ReactNode } from "react";

// Label is the uppercase eyebrow shown above sections and figures.
export function Label({ children }: { children: ReactNode }) {
  return (
    <div className="text-[10px] font-semibold uppercase tracking-[0.16em] text-faint">
      {children}
    </div>
  );
}
