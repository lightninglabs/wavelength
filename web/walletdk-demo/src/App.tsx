import { useCallback, useEffect, useMemo, useState } from "react";
import { useWalletDK } from "@lightninglabs/walletdk-react";
import { AppShell } from "./components/layout/AppShell";
import { AppTab } from "./components/layout/nav";
import { usePasskey } from "./hooks/usePasskey";
import { errorMessage } from "./lib/errors";
import { phaseConnected, statusLabel } from "./lib/phase";
import {
  RuntimeForm,
  signetDefaults,
} from "./lib/runtime-config";
import { HomeScreen } from "./screens/home";
import {
  BackupScreen,
  ConnectScreen,
  CreateScreen,
  ErrorScreen,
  LoadingScreen,
  type LogRow,
  StoppedScreen,
  SyncingScreen,
  UnlockScreen,
} from "./screens/onboarding";
import { ReceiveScreen } from "./screens/receive";
import { SendScreen } from "./screens/send";
import { ActivityScreen } from "./screens/activity";
import { SettingsScreen } from "./screens/settings";

const MAX_LOGS = 8;

// App is the wallet orchestrator: it owns cross-screen session state (runtime
// form, recovery-phrase backup gating, passkey wiring, log tail, active tab) and
// routes to the correct screen by runtime phase. The data layer lives in
// WalletDKProvider; presentational screens receive values + handlers as props.
export function App() {
  const wallet = useWalletDK();
  const passkey = usePasskey();

  const [form, setForm] = useState<RuntimeForm>(signetDefaults);
  const [mnemonic, setMnemonic] = useState<string[]>([]);
  const [backupAcknowledged, setBackupAcknowledged] = useState(false);
  const [logs, setLogs] = useState<LogRow[]>([]);
  const [tab, setTab] = useState<AppTab>("home");
  const [passkeyVersion, setPasskeyVersion] = useState(0);
  const [enrolling, setEnrolling] = useState(false);
  const [unlocking, setUnlocking] = useState(false);

  const phaseLabel = statusLabel(wallet.phase);

  // onField updates a single runtime-config field (connect + settings forms).
  const onField = useCallback(
    <K extends keyof RuntimeForm>(key: K, value: RuntimeForm[K]) => {
      setForm((current) => ({ ...current, [key]: value }));
    },
    [],
  );

  // Tail the runtime log stream so the syncing screen can show live progress.
  useEffect(() => {
    return wallet.client.subscribe((event) => {
      if (event.type !== "log") {
        return;
      }

      const payload = event.payload as { message?: string };
      if (!payload?.message) {
        return;
      }

      setLogs((rows) =>
        [
          { time: new Date().toLocaleTimeString(), message: payload.message! },
          ...rows,
        ].slice(0, MAX_LOGS),
      );
    });
  }, [wallet.client]);

  // While syncing, poll for readiness: the provider only re-derives the phase on
  // refresh, so without this the wallet would never leave the syncing screen.
  useEffect(() => {
    if (wallet.phase !== "syncing") {
      return;
    }

    let active = true;
    const poll = () => {
      if (active) {
        wallet.refresh().catch(() => undefined);
      }
    };
    const id = window.setInterval(poll, 2000);

    return () => {
      active = false;
      window.clearInterval(id);
    };
  }, [wallet.phase, wallet.refresh]);

  // While the wallet is unlocked, poll for balance/activity updates so on-chain
  // deposits, confirmations and incoming payments surface on the dashboard
  // without a manual refresh.
  useEffect(() => {
    if (wallet.phase !== "ready") {
      return;
    }

    let active = true;
    const id = window.setInterval(() => {
      if (active) {
        wallet.refresh().catch(() => undefined);
      }
    }, 10000);

    return () => {
      active = false;
      window.clearInterval(id);
    };
  }, [wallet.phase, wallet.refresh]);

  const passkeyEnrolled = useMemo(
    () => passkey.wrapExists(form.dataDir),
    // passkeyVersion forces re-read after enroll/remove (storage is untracked).
    [passkey, form.dataDir, passkeyVersion, wallet.phase],
  );

  const startRuntime = useCallback(async () => {
    try {
      const info = await wallet.start(form);
      setBackupAcknowledged(Boolean(info.WalletReady));
    } catch {
      // Surfaced via operations.runtime.error / wallet.error.
    }
  }, [wallet, form]);

  const createWallet = useCallback(
    async ({
      password,
      enablePasskey,
    }: {
      password: string;
      enablePasskey: boolean;
    }) => {
      const withPasskey = enablePasskey && passkey.supported;

      // A passkey enrollment shows an OS biometric prompt; keep the UI on a
      // loading screen during create + enroll so the recovery phrase is never
      // revealed behind that prompt.
      if (withPasskey) {
        setEnrolling(true);
      }

      const result = await wallet.createWallet({ password }).catch(() => null);
      if (!result) {
        // Surfaced via operations.createWallet.error.
        setEnrolling(false);

        return;
      }

      if (withPasskey) {
        try {
          await passkey.enroll(form.dataDir, password);
          setPasskeyVersion((v) => v + 1);
        } catch {
          // Enrollment failed/cancelled (surfaced via passkey.error); the wallet
          // still exists, so fall through and reveal the recovery phrase.
        } finally {
          setEnrolling(false);
        }
      }

      // Reveal the recovery phrase only after enrollment settles.
      setMnemonic(result.Mnemonic || []);
      setBackupAcknowledged(false);
    },
    [wallet, passkey, form.dataDir],
  );

  const restoreWallet = useCallback(
    async ({
      password,
      mnemonic: words,
      passphrase,
    }: {
      password: string;
      mnemonic: string[];
      passphrase: string;
    }) => {
      try {
        await wallet.createWallet({
          password,
          mnemonic: words,
          seedPassphrase: passphrase || undefined,
        });
        setMnemonic([]);
        setBackupAcknowledged(true);
      } catch {
        // Surfaced via operations.createWallet.error.
      }
    },
    [wallet],
  );

  const unlockWithPassword = useCallback(
    async (password: string) => {
      try {
        await wallet.unlockWallet({ password });
        setBackupAcknowledged(true);
      } catch {
        // Surfaced via operations.unlockWallet.error.
      }
    },
    [wallet],
  );

  const unlockWithPasskey = useCallback(async () => {
    // Mirror the create flow: hold on a loading screen during the biometric
    // prompt and the decrypt/sync that follows, rather than leaving the unlock
    // form sitting behind the OS prompt.
    setUnlocking(true);
    try {
      const password = await passkey.unlock(form.dataDir);
      await unlockWithPassword(password);
    } catch {
      // Surfaced via passkey.error.
    } finally {
      setUnlocking(false);
    }
  }, [passkey, form.dataDir, unlockWithPassword]);

  const acknowledgeBackup = useCallback(async () => {
    setBackupAcknowledged(true);
    await wallet.refresh().catch(() => undefined);
  }, [wallet]);

  const stopRuntime = useCallback(async () => {
    try {
      await wallet.stop();
      setMnemonic([]);
      setBackupAcknowledged(false);
      setTab("home");
    } catch {
      // Surfaced via operations.runtime.error.
    }
  }, [wallet]);

  const removePasskey = useCallback(() => {
    passkey.remove(form.dataDir);
    setPasskeyVersion((v) => v + 1);
  }, [passkey, form.dataDir]);

  const network = form.network;

  // Passkey enrollment in flight: hold on a loading screen so the freshly
  // generated recovery phrase stays hidden behind the biometric prompt.
  if (enrolling) {
    return (
      <LoadingScreen
        network={network}
        title="Creating wallet"
        sub="Generating keys and registering your passkey."
      />
    );
  }

  // Passkey unlock in flight: hold on a loading screen behind the biometric
  // prompt instead of leaving the unlock form visible underneath it.
  if (unlocking) {
    return (
      <LoadingScreen
        network={network}
        title="Unlocking wallet"
        sub="Decrypting keys and syncing — this can take a few seconds."
      />
    );
  }

  switch (wallet.phase) {
  case "loading":
    return (
      <LoadingScreen
        network={network}
        title="Starting WalletDK"
        sub="Downloading and instantiating the WASM runtime."
      />
    );

  case "starting":
    return (
      <LoadingScreen
        network={network}
        title="Starting runtime"
        sub="Connecting to the gateways."
      />
    );

  case "stopping":
    return (
      <LoadingScreen
        network={network}
        title="Stopping runtime"
        sub="Tearing down the wallet."
      />
    );

  case "runtimeReady":
    return (
      <ConnectScreen
        form={form}
        onField={onField}
        onStart={startRuntime}
        busy={wallet.operations.runtime.busy}
        error={wallet.operations.runtime.error || wallet.error}
      />
    );

  case "needsWallet":
    return (
      <CreateScreen
        network={network}
        passkeySupported={passkey.supported}
        onCreate={createWallet}
        onRestore={restoreWallet}
        busy={wallet.operations.createWallet.busy}
        error={wallet.operations.createWallet.error || passkey.error}
      />
    );

  case "locked":
    return (
      <UnlockScreen
        network={network}
        passkeySupported={passkey.supported}
        passkeyWrapAvailable={passkeyEnrolled}
        onUnlock={unlockWithPassword}
        onUnlockPasskey={unlockWithPasskey}
        busy={wallet.operations.unlockWallet.busy}
        error={wallet.operations.unlockWallet.error}
        passkeyBusy={passkey.busy}
        passkeyError={passkey.error}
      />
    );

  case "syncing":
    return (
      <SyncingScreen
        network={network}
        blockHeight={wallet.info?.BlockHeight}
        logs={logs}
      />
    );

  case "stopped":
    return (
      <StoppedScreen
        network={network}
        onStart={startRuntime}
        busy={wallet.operations.runtime.busy}
        blockHeight={wallet.info?.BlockHeight}
        version={wallet.info?.Version}
      />
    );

  case "error":
    return (
      <ErrorScreen
        network={network}
        message={wallet.error || wallet.operations.runtime.error}
        onRetry={startRuntime}
        busy={wallet.operations.runtime.busy}
      />
    );

  case "ready":
  default:
    break;
  }

  // Freshly created wallet: show the recovery phrase once before the dashboard.
  if (!backupAcknowledged && mnemonic.length > 0) {
    return (
      <BackupScreen
        network={network}
        mnemonic={mnemonic}
        onAcknowledge={acknowledgeBackup}
        busy={wallet.operations.refresh.busy}
      />
    );
  }

  return (
    <AppShell
      tab={tab}
      onTab={setTab}
      onStop={stopRuntime}
      status={{
        phaseLabel,
        network: wallet.info?.Network || network,
        connected: phaseConnected(wallet.phase),
        identityPubKey: wallet.info?.IdentityPubKey || "",
      }}
    >
      {tab === "home" ? (
        <HomeScreen
          balance={wallet.balance}
          activity={wallet.activity}
          info={wallet.info}
          phaseLabel={phaseLabel}
          onNavigate={setTab}
          onDeposit={wallet.deposit}
          depositBusy={wallet.operations.deposit.busy}
          depositError={wallet.operations.deposit.error}
        />
      ) : null}
      {tab === "receive" ? (
        <ReceiveScreen
          onNavigate={setTab}
          onReceive={wallet.receive}
          onDeposit={wallet.deposit}
          receiveBusy={wallet.operations.receive.busy}
          receiveError={wallet.operations.receive.error}
          depositBusy={wallet.operations.deposit.busy}
          depositError={wallet.operations.deposit.error}
        />
      ) : null}
      {tab === "send" ? (
        <SendScreen
          onNavigate={setTab}
          onSend={wallet.send}
          busy={wallet.operations.send.busy}
          error={wallet.operations.send.error}
        />
      ) : null}
      {tab === "activity" ? (
        <ActivityScreen
          activity={wallet.activity}
          onNavigate={setTab}
          onRefresh={() => wallet.refresh().catch(() => undefined)}
          busy={wallet.operations.refresh.busy}
        />
      ) : null}
      {tab === "settings" ? (
        <SettingsScreen
          info={wallet.info}
          phaseLabel={phaseLabel}
          form={form}
          onField={onField}
          passkeySupported={passkey.supported}
          passkeyEnrolled={passkeyEnrolled}
          onRemovePasskey={removePasskey}
          onStop={stopRuntime}
          onNavigate={setTab}
        />
      ) : null}
    </AppShell>
  );
}
