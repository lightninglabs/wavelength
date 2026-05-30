import { Bitcoin, Power, Settings } from "lucide-react";
import { cn } from "../../lib/cn";
import { shortKey } from "../../lib/format";
import { ThemeToggle } from "../ui/ThemeToggle";
import { AppTab, CENTER_NAV, ChromeStatus } from "./nav";

// TopNav is the full-width top navbar, styled to sit with Zones: a square brand
// tile, tonal (not pill) active links, flat hairline status/account groups and
// square icon buttons. Brand on the left, primary nav centre on desktop, and
// runtime status / account / theme / settings / stop on the right.
export function TopNav({
  tab,
  onTab,
  onStop,
  status,
}: {
  tab: AppTab;
  onTab: (tab: AppTab) => void;
  onStop: () => void;
  status: ChromeStatus;
}) {
  return (
    <header className="sticky top-0 z-20 w-full border-b border-border bg-bg">
      <div
        className="mx-auto flex h-16 w-full max-w-6xl items-center gap-3
          px-4 lg:px-8"
      >
        <button
          type="button"
          onClick={() => onTab("home")}
          className="flex shrink-0 items-center gap-2.5"
        >
          <span className="flex h-8 w-8 items-center justify-center bg-accent">
            <Bitcoin size={17} className="text-white" />
          </span>
          <span className="text-sm font-semibold text-fg">WalletDK</span>
        </button>

        <nav className="ml-3 hidden items-center lg:flex">
          {CENTER_NAV.map((n) => {
            const on = tab === n.id;

            return (
              <button
                key={n.id}
                type="button"
                onClick={() => onTab(n.id)}
                className={cn(
                  `flex items-center gap-2 px-3 py-2 text-sm font-medium
                  transition-colors`,
                  on
                    ? "[background:var(--surface-alt)] text-fg"
                    : "text-muted hover:text-fg",
                )}
              >
                <n.icon size={16} />
                {n.label}
              </button>
            );
          })}
        </nav>

        <div className="ml-auto flex items-center gap-3">
          <div className="hidden items-center gap-2 sm:flex">
            <span
              className={cn(
                "h-1.5 w-1.5 rounded-full",
                status.connected ? "bg-good" : "bg-bad",
              )}
            />
            <span className="font-mono text-xs capitalize text-muted">
              {status.phaseLabel}
            </span>
            <span className="text-faint">·</span>
            <span className="font-mono text-xs text-muted">
              {status.network}
            </span>
          </div>

          {status.identityPubKey ? (
            <>
              <span className="hidden h-5 w-px bg-border md:block" />
              <div
                data-testid="account-pubkey"
                data-pubkey={status.identityPubKey}
                className="hidden items-center gap-2 md:flex"
              >
                <span
                  className="flex h-6 w-6 items-center justify-center
                    [background:var(--surface-alt)] text-[11px] font-bold
                    text-accent"
                >
                  DK
                </span>
                <span className="font-mono text-xs font-medium tabular-nums text-muted">
                  {shortKey(status.identityPubKey, 4, 4)}
                </span>
              </div>
            </>
          ) : null}

          <div className="flex items-center gap-1.5">
            <ThemeToggle />

            <button
              type="button"
              title="Settings"
              onClick={() => onTab("settings")}
              className={cn(
                "flex h-9 w-9 items-center justify-center border border-border transition-colors",
                tab === "settings"
                  ? "[background:var(--surface-alt)] text-fg"
                  : "text-muted hover:text-fg",
              )}
            >
              <Settings size={16} />
            </button>

            <button
              type="button"
              title="Stop runtime"
              onClick={onStop}
              className="flex h-9 w-9 items-center justify-center border
                border-border text-muted transition-colors hover:text-fg"
            >
              <Power size={15} />
            </button>
          </div>
        </div>
      </div>
    </header>
  );
}
