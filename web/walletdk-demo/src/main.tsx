import { WalletDKProvider } from "@lightninglabs/walletdk-react";
import React from "react";
import { createRoot } from "react-dom/client";
import { App } from "./App";
import "./styles.css";

createRoot(document.getElementById("root")!).render(
  <React.StrictMode>
    <WalletDKProvider>
      <App />
    </WalletDKProvider>
  </React.StrictMode>,
);
