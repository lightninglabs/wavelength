import React from "react";
import { createRoot } from "react-dom/client";
import { WalletDKProvider } from "@lightninglabs/walletdk-react";
import { App } from "./App";
import { ThemeProvider } from "./theme/ThemeProvider";
import "./index.css";

createRoot(document.getElementById("root")!).render(
  <React.StrictMode>
    <ThemeProvider>
      <WalletDKProvider>
        <App />
      </WalletDKProvider>
    </ThemeProvider>
  </React.StrictMode>,
);
