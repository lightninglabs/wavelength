import { ReactNode } from "react";
import { ThemeToggle } from "../ui/ThemeToggle";
import { BrandPanel } from "./BrandPanel";

// AuthLayout is the split onboarding structure: a brand panel on the left and
// the routed form on the right, stacking on mobile. A theme toggle sits in the
// top-right corner so the palette can be switched before signing in.
export function AuthLayout({
  children,
  network,
  wide = false,
}: {
  children: ReactNode;
  network: string;
  wide?: boolean;
}) {
  return (
    <div className="relative grid min-h-screen w-full bg-bg text-fg lg:grid-cols-2">
      <BrandPanel network={network} />
      <div className="flex items-center justify-center px-4 py-10 sm:px-8">
        <div className="w-full" style={{ maxWidth: wide ? 540 : 420 }}>
          {children}
        </div>
      </div>
      <div className="absolute right-4 top-4 z-10 sm:right-6 sm:top-6">
        <ThemeToggle />
      </div>
    </div>
  );
}
