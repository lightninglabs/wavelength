import { ReactNode } from "react";
import { AppTab, ChromeStatus } from "./nav";
import { TopNav } from "./TopNav";
import { BottomBar } from "./BottomBar";

// AppShell is the authenticated app frame: a full-width top navbar, the routed
// screen, and a mobile bottom tab bar.
export function AppShell({
  tab,
  onTab,
  onStop,
  status,
  children,
}: {
  tab: AppTab;
  onTab: (tab: AppTab) => void;
  onStop: () => void;
  status: ChromeStatus;
  children: ReactNode;
}) {
  return (
    <div className="flex min-h-screen w-full flex-col bg-bg text-fg">
      <TopNav tab={tab} onTab={onTab} onStop={onStop} status={status} />
      <main className="w-full flex-1 pb-24 lg:pb-12">{children}</main>
      <BottomBar tab={tab} onTab={onTab} />
    </div>
  );
}
