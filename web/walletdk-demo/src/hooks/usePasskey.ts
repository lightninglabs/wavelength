import { useCallback, useEffect, useState } from "react";
import {
  clearPasskeyWrap,
  createPasskeyWrap,
  hasPasskeyWrap,
  unwrapPasskeyPassword,
  supportsPasskeyPrf,
} from "@lightninglabs/walletdk-wasm-web";
import { errorMessage } from "../lib/errors";

const APP_NAME = "Dare Wallet";

export type UsePasskey = {
  // supported reports WebAuthn PRF availability (probed once on mount).
  supported: boolean;
  // busy is true while a passkey ceremony (enroll/unlock) is in flight.
  busy: boolean;
  // error holds the last passkey failure message, or "".
  error: string;
  // enroll registers a passkey wrapping the wallet password. Returns true on
  // success; on failure it records `error` and returns false (non-fatal).
  enroll: (dataDir: string, password: string) => Promise<boolean>;
  // unlock authenticates with the stored passkey and resolves the recovered
  // wallet password. Rejects (and records `error`) on failure.
  unlock: (dataDir: string) => Promise<string>;
  // wrapExists reports whether a passkey wrap is stored for the data dir.
  wrapExists: (dataDir: string) => boolean;
  // remove deletes the stored passkey wrap for the data dir.
  remove: (dataDir: string) => void;
  // clearError resets the last error.
  clearError: () => void;
};

// usePasskey wraps the wasm-web passkey helpers with React state for support
// detection, in-flight status and error reporting.
export function usePasskey(): UsePasskey {
  const [supported, setSupported] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");

  useEffect(() => {
    let cancelled = false;

    supportsPasskeyPrf().then((value) => {
      if (!cancelled) {
        setSupported(value);
      }
    });

    return () => {
      cancelled = true;
    };
  }, []);

  const enroll = useCallback(async (dataDir: string, password: string) => {
    setError("");
    setBusy(true);

    try {
      await createPasskeyWrap(dataDir, password, { appName: APP_NAME });

      return true;
    } catch (err) {
      setError(errorMessage(err));

      return false;
    } finally {
      setBusy(false);
    }
  }, []);

  const unlock = useCallback(async (dataDir: string) => {
    setError("");
    setBusy(true);

    try {
      return await unwrapPasskeyPassword(dataDir);
    } catch (err) {
      setError(errorMessage(err));
      throw err;
    } finally {
      setBusy(false);
    }
  }, []);

  const wrapExists = useCallback((dataDir: string) => {
    return hasPasskeyWrap(dataDir);
  }, []);

  const remove = useCallback((dataDir: string) => {
    clearPasskeyWrap(dataDir);
  }, []);

  const clearError = useCallback(() => setError(""), []);

  return {
    supported,
    busy,
    error,
    enroll,
    unlock,
    wrapExists,
    remove,
    clearError,
  };
}
