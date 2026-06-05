import React from "react";
import { createRoot } from "react-dom/client";
import { WalletDKProvider } from "@lightninglabs/walletdk-react";
import { App } from "./App";
import { ThemeProvider } from "./theme/ThemeProvider";
import { consumePendingWipe } from "./lib/wipeLocalData";
import "./index.css";

// boot clears any pending wipe before mounting, so a reset starts the app from
// clean storage with no OPFS handles held open.
async function boot() {
  await consumePendingWipe();

  createRoot(document.getElementById("root")!).render(
    <React.StrictMode>
      <ThemeProvider>
        <WalletDKProvider>
          <App />
        </WalletDKProvider>
      </ThemeProvider>
    </React.StrictMode>,
  );
}

void boot();
