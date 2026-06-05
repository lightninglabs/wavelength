import { useCallback, useEffect, useState } from "react";
import {
  assertPasskeyPrf,
  registerPasskeyWallet,
  supportsPasskeyPrf,
} from "@lightninglabs/walletdk-wasm-web";
import type {
  OpenWalletFromPasskeyResult, 
  WalletDKClient,
} from "@lightninglabs/walletdk-core";

// PasskeyWalletOutcome pairs the Go-side open result with the credential id that
// was used, so the app can persist it and scope future unlocks.
export type PasskeyWalletOutcome = {
  result: OpenWalletFromPasskeyResult;
  credentialId: string;
};

export type UsePasskeyWallet = {
  supported: boolean;
  busy: boolean;
  error: string;
  // createPasskeyWallet registers a passkey and creates the wallet from it.
  createPasskeyWallet: (
    client: WalletDKClient,
    appName: string,
  ) => Promise<PasskeyWalletOutcome | null>;
  // openPasskeyWallet asserts a passkey (scoped when allowCredentialId is set,
  // discoverable otherwise) and imports/unlocks the wallet.
  openPasskeyWallet: (
    client: WalletDKClient,
    allowCredentialId?: string,
  ) => Promise<PasskeyWalletOutcome | null>;
  clearError: () => void;
};

// usePasskeyWallet wraps the passkey ceremony plus the Go-side open in React
// state for support detection, in-flight status and error reporting.
export function usePasskeyWallet(): UsePasskeyWallet {
  const [supported, setSupported] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");

  useEffect(() => {
    let cancelled = false;
    supportsPasskeyPrf().then((v) => {
      if (!cancelled) setSupported(v);
    });
    return () => {
      cancelled = true;
    };
  }, []);

  const run = useCallback(
    async <T,>(fn: () => Promise<T>): Promise<T | null> => {
      setError("");
      setBusy(true);
      try {
        return await fn();
      } catch (err) {
        setError(err instanceof Error ? err.message : String(err));
        return null;
      } finally {
        setBusy(false);
      }
    },
    [],
  );

  const createPasskeyWallet = useCallback(
    (client: WalletDKClient, appName: string) =>
      run(async () => {
        const { prfOutput, credentialId } = await registerPasskeyWallet(appName);
        const result = await client.openWalletFromPasskey({ prfOutput });
        return { result, credentialId };
      }),
    [run],
  );

  const openPasskeyWallet = useCallback(
    (client: WalletDKClient, allowCredentialId?: string) =>
      run(async () => {
        const { prfOutput, credentialId } = await assertPasskeyPrf(allowCredentialId);
        const result = await client.openWalletFromPasskey({ prfOutput });
        return { result, credentialId };
      }),
    [run],
  );

  const clearError = useCallback(() => setError(""), []);

  return {
    supported,
    busy,
    error,
    createPasskeyWallet,
    openPasskeyWallet,
    clearError,
  };
}
