import {
  Activity,
  ArrowDownLeft,
  ArrowUpRight,
  LayoutGrid,
  type LucideIcon,
  Settings,
} from "lucide-react";

// AppTab is an authenticated (post-ready) destination. The runtime-lifecycle
// screens are NOT tabs - they are selected by wallet phase.
export type AppTab = "home" | "receive" | "send" | "activity" | "settings";

// NAV is the primary navigation, shared by the top navbar and the mobile
// bottom bar. "Overview" maps to the home dashboard.
export const NAV: Array<{ id: AppTab; label: string; icon: LucideIcon }> = [
  { id: "home", label: "Overview", icon: LayoutGrid },
  { id: "activity", label: "Activity", icon: Activity },
  { id: "receive", label: "Receive", icon: ArrowDownLeft },
  { id: "send", label: "Send", icon: ArrowUpRight },
  { id: "settings", label: "Settings", icon: Settings },
];

// CENTER_NAV is the desktop top-navbar links. Settings lives as a dedicated
// right-side icon button, so it is excluded here.
export const CENTER_NAV = NAV.filter((n) => n.id !== "settings");

// ChromeStatus is the runtime/account summary the chrome renders in the top
// navbar status pill and account chip.
export type ChromeStatus = {
  phaseLabel: string;
  network: string;
  connected: boolean;
  identityPubKey: string;
};
