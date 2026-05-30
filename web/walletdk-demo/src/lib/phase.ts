import { RuntimePhase } from "@lightninglabs/walletdk-react";

// statusLabel maps a runtime phase to a concise label for the status pill and
// settings/home runtime cards.
export function statusLabel(phase: RuntimePhase): string {
  switch (phase) {
  case "loading":
    return "loading";
  case "runtimeReady":
    return "ready";
  case "starting":
    return "starting";
  case "needsWallet":
    return "setup";
  case "locked":
    return "locked";
  case "syncing":
    return "syncing";
  case "ready":
    return "ready";
  case "stopping":
    return "stopping";
  case "stopped":
    return "stopped";
  default:
    return "error";
  }
}

// phaseConnected reports whether the runtime is connected to its gateways for
// the chrome status indicator.
export function phaseConnected(phase: RuntimePhase): boolean {
  return phase === "ready" || phase === "syncing";
}
