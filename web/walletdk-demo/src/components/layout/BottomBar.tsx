import { cn } from "../../lib/cn";
import { AppTab, NAV } from "./nav";

// BottomBar is the mobile-only tab bar that replaces the desktop top-nav links.
export function BottomBar({
  tab,
  onTab,
}: {
  tab: AppTab;
  onTab: (tab: AppTab) => void;
}) {
  return (
    <nav
      className="sticky bottom-0 z-10 flex items-center justify-around border-t
        border-border bg-bg/90 px-2 py-2 backdrop-blur lg:hidden"
    >
      {NAV.map((n) => {
        const on = tab === n.id;

        return (
          <button
            key={n.id}
            type="button"
            onClick={() => onTab(n.id)}
            className={cn(
              `flex flex-1 flex-col items-center gap-1 py-1.5 text-[10px]
              font-medium transition-colors`,
              on
                ? "[background:var(--surface-alt)] text-fg"
                : "text-muted",
            )}
          >
            <n.icon size={18} />
            {n.label}
          </button>
        );
      })}
    </nav>
  );
}
