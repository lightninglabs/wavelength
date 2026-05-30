import { Bitcoin } from "lucide-react";

// AuthHeader is the heading block atop each onboarding form, with a mobile-only
// brand lockup (the brand panel is hidden on small screens).
export function AuthHeader({ title, sub }: { title: string; sub: string }) {
  return (
    <div className="mb-6">
      <div className="mb-5 flex items-center gap-2.5 lg:hidden">
        <span className="flex h-8 w-8 items-center justify-center bg-accent">
          <Bitcoin size={16} className="text-white" />
        </span>
        <span className="text-sm font-semibold text-fg">WalletDK</span>
      </div>
      <h1 className="text-xl font-semibold text-fg">{title}</h1>
      <p className="mt-1 text-sm text-muted">{sub}</p>
    </div>
  );
}
