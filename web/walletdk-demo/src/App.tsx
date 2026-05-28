import {
  Activity,
  ArrowDownToLine,
  ArrowUpFromLine,
  CircleStop,
  Copy,
  Download,
  Eye,
  KeyRound,
  LoaderCircle,
  Play,
  Plus,
  RefreshCw,
  Send,
  Settings,
  Wallet,
} from "lucide-react";
import {
  FormEvent,
  ReactNode,
  useEffect,
  useMemo,
  useState,
} from "react";
import {
  Entry,
  RuntimeConfig,
} from "@lightninglabs/walletdk-core";
import {
  RuntimePhase,
  WalletOperation,
  useWalletDK,
} from "@lightninglabs/walletdk-react";
import {
  createPasskeyWrap,
  hasPasskeyWrap,
  supportsPasskeyPrf,
  unwrapPasskeyPassword,
} from "@lightninglabs/walletdk-wasm-web";

const signetDefaults: Required<RuntimeConfig> = {
  network: "signet",
  dataDir: "/walletdk-demo",
  arkGatewayURL: "https://arkd-signet-rest.testnet.lightningcluster.com",
  mailboxGatewayURL: "https://arkd-signet-rest.testnet.lightningcluster.com",
  walletEsploraURL: "https://mempool-signet.testnet.lightningcluster.com/api",
  swapServerGatewayURL: "https://swapd-signet-rest.testnet.lightningcluster.com",
  swapMailboxGatewayURL: "https://swapd-signet-rest.testnet.lightningcluster.com",
  swapDatabaseFileName: "/walletdk-swaps.db",
  serverInsecure: false,
  swapServerInsecure: false,
  disableSwaps: false,
  debugLevel: "info",
};

type RuntimeForm = Required<RuntimeConfig>;

type LogRow = {
  time: string;
  message: string;
};

type WalletTab = "home" | "receive" | "send" | "activity";

export function App() {
  const wallet = useWalletDK();
  const [runtimeForm, setRuntimeForm] = useState<RuntimeForm>(signetDefaults);
  const [logs, setLogs] = useState<LogRow[]>([]);
  const [password, setPassword] = useState("");
  const [enablePasskeyOnCreate, setEnablePasskeyOnCreate] = useState(false);
  const [passkeySupported, setPasskeySupported] = useState(false);
  const [passkeyBusy, setPasskeyBusy] = useState(false);
  const [passkeyError, setPasskeyError] = useState("");
  const [mnemonic, setMnemonic] = useState<string[]>([]);
  const [mnemonicAcknowledged, setMnemonicAcknowledged] = useState(false);
  const [address, setAddress] = useState("");
  const [receiveAmount, setReceiveAmount] = useState("1000");
  const [receiveMemo, setReceiveMemo] = useState("walletdk receive");
  const [invoice, setInvoice] = useState("");
  const [sendInvoice, setSendInvoice] = useState("");
  const [sendMaxFee, setSendMaxFee] = useState("0");
  const [sendHash, setSendHash] = useState("");
  const [activeTab, setActiveTab] = useState<WalletTab>("receive");

  const runtimeStarted = isRuntimeStarted(wallet.phase);
  const walletReady = wallet.phase === "ready";
  const needsBootstrap = runtimeStarted && !walletReady;
  const showDashboard = walletReady && mnemonicAcknowledged;
  const busy = busyOperation(wallet.operations);
  const statusText = statusLabel(wallet.phase);
  const runtimeBusy = wallet.operations.runtime.busy;
  const passkeyWrapAvailable = useMemo(() => {
    return hasPasskeyWrap(runtimeForm.dataDir);
  }, [runtimeForm.dataDir, wallet.phase]);

  useEffect(() => {
    let cancelled = false;

    supportsPasskeyPrf().then((supported) => {
      if (cancelled) {
        return;
      }

      setPasskeySupported(supported);
      if (supported) {
        setEnablePasskeyOnCreate(true);
      }
    });

    return () => {
      cancelled = true;
    };
  }, []);

  useEffect(() => {
    return wallet.client.subscribe((event) => {
      if (event.type !== "log") {
        return;
      }

      const payload = event.payload as { message?: string };
      if (payload?.message) {
        log(payload.message);
      }
    });
  }, [wallet.client]);

  const facts = useMemo(() => {
    return {
      network: wallet.info?.Network || "",
      height: wallet.info?.BlockHeight ?? "",
      "server connected": String(Boolean(wallet.info?.ServerConnected)),
      "wallet ready": String(walletReady),
      "wallet type": wallet.info?.WalletType || "",
      "identity pubkey": wallet.info?.IdentityPubKey || "",
    };
  }, [wallet.info, walletReady]);

  const balanceFacts = useMemo(() => {
    const balance = wallet.balance || {};

    return {
      "boarding confirmed": balance.BoardingConfirmedSat ??
        balance.ConfirmedSat ?? "",
      "boarding unconfirmed": balance.BoardingUnconfirmedSat ??
        balance.PendingInSat ?? "",
      "vtxo balance": balance.VTXOBalanceSat ?? balance.ConfirmedSat ?? "",
      "total confirmed": totalBalance(wallet.balance),
      "on-chain wallet": balance.OnchainWalletConfirmedSat ??
        balance.ConfirmedSat ?? "",
    };
  }, [wallet.balance]);

  function log(message: string) {
    console.log(message);
    setLogs((rows) => [
      {
        time: new Date().toLocaleTimeString(),
        message,
      },
      ...rows,
    ].slice(0, 50));
  }

  function updateRuntime<K extends keyof RuntimeForm>(
    key: K,
    value: RuntimeForm[K],
  ) {
    setRuntimeForm((current) => ({
      ...current,
      [key]: value,
    }));
  }

  async function startRuntime(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();

    try {
      const info = await wallet.start(runtimeForm);
      setMnemonicAcknowledged(Boolean(info.WalletReady));
      log("runtime started");
    } catch (err) {
      log(errorMessage(err));
    }
  }

  async function createWallet(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    setPasskeyError("");

    try {
      const result = await wallet.createWallet({ password });
      setMnemonic(result.Mnemonic || []);
      setMnemonicAcknowledged(false);
      log(`wallet created ${result.IdentityPubKey}`);

      if (enablePasskeyOnCreate && passkeySupported) {
        try {
          await createPasskeyWrap(runtimeForm.dataDir, password, {
            appName: "Dare Wallet",
          });
          log("passkey unlock enabled");
        } catch (err) {
          const message = errorMessage(err);
          setPasskeyError(message);
          log(`passkey setup failed: ${message}`);
        }
      }
    } catch (err) {
      log(errorMessage(err));
    }
  }

  async function unlockWallet(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    setPasskeyError("");

    try {
      await unlockWithPassword(password);
    } catch (err) {
      log(errorMessage(err));
    }
  }

  async function unlockWalletWithPasskey() {
    setPasskeyError("");
    setPasskeyBusy(true);

    try {
      const recoveredPassword = await unwrapPasskeyPassword(
        runtimeForm.dataDir,
      );
      await unlockWithPassword(recoveredPassword);
      log("wallet unlocked with passkey");
    } catch (err) {
      const message = errorMessage(err);
      setPasskeyError(message);
      log(`passkey unlock failed: ${message}`);
    } finally {
      setPasskeyBusy(false);
    }
  }

  async function unlockWithPassword(unlockPassword: string) {
    const result = await wallet.unlockWallet({ password: unlockPassword });
    setMnemonicAcknowledged(true);
    log(`wallet unlocked ${result.IdentityPubKey}`);
    await waitForWalletReady();
    await wallet.refresh();
  }

  async function waitForWalletReady() {
    for (let i = 0; i < 120; i++) {
      const info = await wallet.client.getInfo();
      if (info.WalletReady && info.IdentityPubKey) {
        return;
      }
      await delay(500);
    }

    throw new Error("wallet did not become ready");
  }

  async function refreshAll() {
    try {
      await wallet.refresh();
      log("wallet refreshed");
    } catch (err) {
      log(errorMessage(err));
    }
  }

  async function stopRuntime() {
    try {
      await wallet.stop();
      setMnemonic([]);
      setMnemonicAcknowledged(false);
      setAddress("");
      setInvoice("");
      setSendHash("");
      setActiveTab("receive");
      log("runtime stopped");
    } catch (err) {
      log(errorMessage(err));
    }
  }

  async function createAddress(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();

    try {
      const nextAddress = await wallet.deposit();
      setAddress(nextAddress);
      log("created onboarding address");
    } catch (err) {
      log(errorMessage(err));
    }
  }

  async function receive(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();

    try {
      const nextInvoice = await wallet.receive({
        amountSat: Number(receiveAmount),
        memo: receiveMemo,
      });
      setInvoice(nextInvoice);
      log("receive swap created");
      setActiveTab("activity");
    } catch (err) {
      log(errorMessage(err));
    }
  }

  async function send(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();

    try {
      const paymentHash = await wallet.send({
        invoice: sendInvoice,
        maxFeeSat: Number(sendMaxFee),
      });
      setSendHash(paymentHash);
      log(`send swap ${paymentHash}`);
      setActiveTab("activity");
    } catch (err) {
      log(errorMessage(err));
    }
  }

  return (
    <div className="wallet-app">
      <header className="topbar">
        <div className="brand-lockup">
          <div className="brand-mark">
            <Wallet size={20} />
          </div>
          <div>
            <h1>Dare Wallet</h1>
            <p>{runtimeForm.network}</p>
          </div>
        </div>
        <div id="runtime-status" className={`status status-${wallet.phase}`}>
          {busy ? <LoaderCircle size={15} className="spin" /> : null}
          {statusText}
        </div>
      </header>

      <main className="wallet-main">
        <section
          id="setup-view"
          className="setup-view"
          hidden={showDashboard}
        >
          <RuntimeCard
            form={runtimeForm}
            runtimeBusy={runtimeBusy}
            updateRuntime={updateRuntime}
            onSubmit={startRuntime}
          />

          <WalletSetup
            needsBootstrap={needsBootstrap}
            phase={wallet.phase}
            password={password}
            setPassword={setPassword}
            passkeySupported={passkeySupported}
            enablePasskeyOnCreate={enablePasskeyOnCreate}
            setEnablePasskeyOnCreate={setEnablePasskeyOnCreate}
            passkeyWrapAvailable={passkeyWrapAvailable}
            passkeyBusy={passkeyBusy}
            passkeyError={passkeyError}
            createBusy={wallet.operations.createWallet.busy}
            unlockBusy={wallet.operations.unlockWallet.busy}
            createError={wallet.operations.createWallet.error}
            unlockError={wallet.operations.unlockWallet.error}
            onCreate={createWallet}
            onUnlock={unlockWallet}
            onUnlockPasskey={unlockWalletWithPasskey}
          />

          <MnemonicBackup
            mnemonic={mnemonic}
            acknowledged={mnemonicAcknowledged}
            onAcknowledge={async () => {
              setMnemonicAcknowledged(true);
              await wallet.refresh().catch((err) => log(errorMessage(err)));
            }}
          />
        </section>

        <section
          id="dashboard-view"
          className="wallet-dashboard"
          hidden={!showDashboard}
        >
          <WalletHome
            balanceSat={totalBalance(wallet.balance)}
            infoFacts={facts}
            balanceFacts={balanceFacts}
            activity={wallet.activity}
            onRefresh={refreshAll}
            onStop={stopRuntime}
            onSelectTab={setActiveTab}
          />

          <section className="wallet-workspace">
            <WalletTabs active={activeTab} setActive={setActiveTab} />
            <div className="workspace-panel">
              <div hidden={activeTab !== "home"}>
                <QuickActions onSelectTab={setActiveTab} />
              </div>

              <div hidden={activeTab !== "receive"}>
                <ReceivePanel
                  address={address}
                  invoice={invoice}
                  receiveAmount={receiveAmount}
                  receiveMemo={receiveMemo}
                  depositBusy={wallet.operations.deposit.busy}
                  receiveBusy={wallet.operations.receive.busy}
                  depositError={wallet.operations.deposit.error}
                  receiveError={wallet.operations.receive.error}
                  setReceiveAmount={setReceiveAmount}
                  setReceiveMemo={setReceiveMemo}
                  onCreateAddress={createAddress}
                  onReceive={receive}
                />
              </div>

              <div hidden={activeTab !== "send"}>
                <SendPanel
                  sendHash={sendHash}
                  sendInvoice={sendInvoice}
                  sendMaxFee={sendMaxFee}
                  sendBusy={wallet.operations.send.busy}
                  sendError={wallet.operations.send.error}
                  setSendInvoice={setSendInvoice}
                  setSendMaxFee={setSendMaxFee}
                  onSend={send}
                />
              </div>

              <div hidden={activeTab !== "activity"}>
                <ActivityPanel
                  entries={wallet.activity}
                  logs={logs}
                  onRefresh={refreshAll}
                />
              </div>
            </div>
          </section>
        </section>
      </main>
    </div>
  );
}

function RuntimeCard({
  form,
  runtimeBusy,
  updateRuntime,
  onSubmit,
}: {
  form: RuntimeForm;
  runtimeBusy: boolean;
  updateRuntime<K extends keyof RuntimeForm>(
    key: K,
    value: RuntimeForm[K],
  ): void;
  onSubmit(event: FormEvent<HTMLFormElement>): void;
}) {
  return (
    <form id="runtime-form" className="wallet-card runtime-card" onSubmit={onSubmit}>
      <div className="card-heading">
        <div>
          <span className="eyebrow">Runtime</span>
          <h2>Start wallet</h2>
        </div>
        <button type="submit" disabled={runtimeBusy}>
          <Play size={16} />
          Start runtime
        </button>
      </div>

      <div className="runtime-summary">
        <SummaryPill label="Network" value={form.network} />
        <SummaryPill label="Ark" value={hostname(form.arkGatewayURL)} />
        <SummaryPill label="Swap" value={hostname(form.swapServerGatewayURL)} />
      </div>

      <details className="advanced-settings">
        <summary>
          <Settings size={16} />
          Advanced settings
        </summary>

        <div className="settings-grid">
          <TextField
            label="Network"
            name="network"
            value={form.network}
            onChange={(value) => updateRuntime("network", value)}
          />
          <TextField
            label="Data dir"
            name="dataDir"
            value={form.dataDir}
            onChange={(value) => updateRuntime("dataDir", value)}
          />
          <TextField
            label="Ark gateway URL"
            name="arkGatewayURL"
            value={form.arkGatewayURL}
            onChange={(value) => updateRuntime("arkGatewayURL", value)}
          />
          <TextField
            label="Mailbox gateway URL"
            name="mailboxGatewayURL"
            value={form.mailboxGatewayURL}
            onChange={(value) => updateRuntime("mailboxGatewayURL", value)}
          />
          <TextField
            label="Esplora URL"
            name="walletEsploraURL"
            value={form.walletEsploraURL}
            onChange={(value) => updateRuntime("walletEsploraURL", value)}
          />
          <TextField
            label="Swap server gateway URL"
            name="swapServerGatewayURL"
            value={form.swapServerGatewayURL}
            onChange={(value) => updateRuntime("swapServerGatewayURL", value)}
          />
          <TextField
            label="Swap mailbox gateway URL"
            name="swapMailboxGatewayURL"
            value={form.swapMailboxGatewayURL}
            onChange={(value) => {
              updateRuntime("swapMailboxGatewayURL", value);
            }}
          />
          <TextField
            label="Swap DB"
            name="swapDatabaseFileName"
            value={form.swapDatabaseFileName}
            onChange={(value) => {
              updateRuntime("swapDatabaseFileName", value);
            }}
          />
        </div>

        <div className="inline">
          <CheckField
            label="Insecure Ark transport"
            name="serverInsecure"
            checked={form.serverInsecure}
            onChange={(value) => updateRuntime("serverInsecure", value)}
          />
          <CheckField
            label="Insecure swap transport"
            name="swapServerInsecure"
            checked={form.swapServerInsecure}
            onChange={(value) => updateRuntime("swapServerInsecure", value)}
          />
          <CheckField
            label="Wallet only"
            name="disableSwaps"
            checked={form.disableSwaps}
            onChange={(value) => updateRuntime("disableSwaps", value)}
          />
        </div>
      </details>
    </form>
  );
}

function WalletSetup({
  createBusy,
  createError,
  enablePasskeyOnCreate,
  needsBootstrap,
  onCreate,
  onUnlock,
  onUnlockPasskey,
  passkeyBusy,
  passkeyError,
  passkeySupported,
  passkeyWrapAvailable,
  password,
  phase,
  setEnablePasskeyOnCreate,
  setPassword,
  unlockBusy,
  unlockError,
}: {
  createBusy: boolean;
  createError: string;
  enablePasskeyOnCreate: boolean;
  needsBootstrap: boolean;
  onCreate(event: FormEvent<HTMLFormElement>): void;
  onUnlock(event: FormEvent<HTMLFormElement>): void;
  onUnlockPasskey(): void | Promise<void>;
  passkeyBusy: boolean;
  passkeyError: string;
  passkeySupported: boolean;
  passkeyWrapAvailable: boolean;
  password: string;
  phase: RuntimePhase;
  setEnablePasskeyOnCreate(value: boolean): void;
  setPassword(value: string): void;
  unlockBusy: boolean;
  unlockError: string;
}) {
  return (
    <div className="setup-stack" hidden={!needsBootstrap}>
      {phase === "needsWallet" ? (
        <form
          id="create-form"
          className="wallet-card setup-card"
          onSubmit={onCreate}
        >
          <CardTitle icon={<Plus size={18} />} title="Create wallet" />
          <PasswordField
            autoComplete="new-password"
            value={password}
            onChange={setPassword}
          />
          {passkeySupported ? (
            <CheckField
              label="Enable passkey unlock (Touch ID / Face ID)"
              name="enablePasskey"
              checked={enablePasskeyOnCreate}
              onChange={setEnablePasskeyOnCreate}
            />
          ) : null}
          <button type="submit" disabled={createBusy}>
            <KeyRound size={16} />
            Create wallet
          </button>
          <InlineError message={createError} />
          <InlineError message={passkeyError} />
        </form>
      ) : null}

      {phase === "locked" ? (
        <form
          id="unlock-form"
          className="wallet-card setup-card"
          onSubmit={onUnlock}
        >
          <CardTitle icon={<KeyRound size={18} />} title="Wallet locked" />
          <PasswordField
            autoComplete="current-password"
            value={password}
            onChange={setPassword}
          />
          <button type="submit" disabled={unlockBusy || passkeyBusy}>
            <KeyRound size={16} />
            Unlock wallet
          </button>
          {passkeySupported && passkeyWrapAvailable ? (
            <>
              <p className="setup-copy passkey-divider">or</p>
              <button
                type="button"
                disabled={unlockBusy || passkeyBusy}
                onClick={onUnlockPasskey}
              >
                <KeyRound size={16} />
                Unlock with passkey
              </button>
            </>
          ) : null}
          <InlineError message={unlockError} />
          <InlineError message={passkeyError} />
        </form>
      ) : null}

      {phase === "syncing" ? (
        <section className="wallet-card setup-card">
          <CardTitle icon={<LoaderCircle size={18} />} title="Wallet syncing" />
          <p className="setup-copy">
            The wallet exists and is syncing before it can be used.
          </p>
        </section>
      ) : null}
    </div>
  );
}

function MnemonicBackup({
  acknowledged,
  mnemonic,
  onAcknowledge,
}: {
  acknowledged: boolean;
  mnemonic: string[];
  onAcknowledge(): void | Promise<void>;
}) {
  return (
    <section
      id="mnemonic-panel"
      className="wallet-card mnemonic-card"
      hidden={mnemonic.length === 0 || acknowledged}
    >
      <CardTitle icon={<KeyRound size={18} />} title="Mnemonic backup" />
      <p id="mnemonic">{mnemonic.join(" ")}</p>
      <button id="mnemonic-ack" type="button" onClick={onAcknowledge}>
        <Copy size={16} />
        I recorded the demo mnemonic
      </button>
    </section>
  );
}

function WalletHome({
  activity,
  balanceFacts,
  balanceSat,
  infoFacts,
  onRefresh,
  onSelectTab,
  onStop,
}: {
  activity: Entry[];
  balanceFacts: Record<string, string | number>;
  balanceSat: number;
  infoFacts: Record<string, string | number>;
  onRefresh(): void;
  onSelectTab(tab: WalletTab): void;
  onStop(): void;
}) {
  const recent = activity.slice(0, 3);

  return (
    <section className="wallet-home">
      <div className="balance-card">
        <div>
          <span className="eyebrow">Available balance</span>
          <div className="balance-amount">{formatSats(balanceSat)}</div>
        </div>
        <div className="balance-actions">
          <button type="button" onClick={() => onSelectTab("receive")}>
            <ArrowDownToLine size={16} />
            Receive
          </button>
          <button type="button" onClick={() => onSelectTab("send")}>
            <ArrowUpFromLine size={16} />
            Send
          </button>
        </div>
      </div>

      <div className="wallet-card facts-card">
        <CardTitle icon={<Eye size={18} />} title="Overview" />
        <Facts id="info-grid" facts={infoFacts} />
      </div>

      <div className="wallet-card facts-card">
        <CardTitle icon={<Wallet size={18} />} title="Balance" />
        <Facts id="balance-grid" facts={balanceFacts} />
      </div>

      <div className="wallet-card recent-card">
        <div className="card-heading compact">
          <CardTitle icon={<Activity size={18} />} title="Recent activity" />
          <button type="button" onClick={onRefresh}>
            <RefreshCw size={16} />
            Refresh
          </button>
        </div>
        {recent.length === 0 ? (
          <EmptyState text="No wallet activity yet." />
        ) : (
          <EntryList entries={recent} />
        )}
        <button type="button" onClick={onStop}>
          <CircleStop size={16} />
          Stop runtime
        </button>
      </div>
    </section>
  );
}

function WalletTabs({
  active,
  setActive,
}: {
  active: WalletTab;
  setActive(tab: WalletTab): void;
}) {
  const tabs: Array<[WalletTab, ReactNode, string]> = [
    ["home", <Wallet size={17} />, "Home"],
    ["receive", <Download size={17} />, "Receive"],
    ["send", <Send size={17} />, "Send"],
    ["activity", <Activity size={17} />, "Activity"],
  ];

  return (
    <nav className="wallet-tabs" aria-label="Wallet views">
      {tabs.map(([tab, icon, label]) => (
        <button
          key={tab}
          type="button"
          className={active === tab ? "active" : ""}
          onClick={() => setActive(tab)}
        >
          {icon}
          {label}
        </button>
      ))}
    </nav>
  );
}

function QuickActions({ onSelectTab }: { onSelectTab(tab: WalletTab): void }) {
  return (
    <section className="wallet-card quick-card">
      <CardTitle icon={<Wallet size={18} />} title="Wallet actions" />
      <div className="action-grid">
        <button type="button" onClick={() => onSelectTab("receive")}>
          <Download size={16} />
          New address
        </button>
        <button type="button" onClick={() => onSelectTab("receive")}>
          <Plus size={16} />
          Create invoice
        </button>
        <button type="button" onClick={() => onSelectTab("send")}>
          <Send size={16} />
          Send payment
        </button>
        <button type="button" onClick={() => onSelectTab("activity")}>
          <Activity size={16} />
          Refresh swaps
        </button>
      </div>
    </section>
  );
}

function ReceivePanel({
  address,
  depositBusy,
  depositError,
  invoice,
  onCreateAddress,
  onReceive,
  receiveAmount,
  receiveBusy,
  receiveError,
  receiveMemo,
  setReceiveAmount,
  setReceiveMemo,
}: {
  address: string;
  depositBusy: boolean;
  depositError: string;
  invoice: string;
  onCreateAddress(event: FormEvent<HTMLFormElement>): void;
  onReceive(event: FormEvent<HTMLFormElement>): void;
  receiveAmount: string;
  receiveBusy: boolean;
  receiveError: string;
  receiveMemo: string;
  setReceiveAmount(value: string): void;
  setReceiveMemo(value: string): void;
}) {
  return (
    <div className="flow-grid">
      <form
        id="address-form"
        className="wallet-card action-card"
        onSubmit={onCreateAddress}
      >
        <CardTitle icon={<Download size={18} />} title="Onboarding address" />
        <button type="submit" disabled={depositBusy}>
          <Plus size={16} />
          New address
        </button>
        <output id="address-output">{address}</output>
        <InlineError message={depositError} />
      </form>

      <form
        id="receive-form"
        className="wallet-card action-card"
        onSubmit={onReceive}
      >
        <CardTitle icon={<Download size={18} />} title="Receive" />
        <TextField
          label="Amount sats"
          name="amountSat"
          type="number"
          min="1"
          step="1"
          value={receiveAmount}
          onChange={setReceiveAmount}
        />
        <TextField
          label="Memo"
          name="memo"
          value={receiveMemo}
          onChange={setReceiveMemo}
        />
        <button type="submit" disabled={receiveBusy}>
          <Plus size={16} />
          Create invoice
        </button>
        <output id="receive-output">{invoice}</output>
        <InlineError message={receiveError} />
      </form>
    </div>
  );
}

function SendPanel({
  onSend,
  sendBusy,
  sendError,
  sendHash,
  sendInvoice,
  sendMaxFee,
  setSendInvoice,
  setSendMaxFee,
}: {
  onSend(event: FormEvent<HTMLFormElement>): void;
  sendBusy: boolean;
  sendError: string;
  sendHash: string;
  sendInvoice: string;
  sendMaxFee: string;
  setSendInvoice(value: string): void;
  setSendMaxFee(value: string): void;
}) {
  return (
    <form id="send-form" className="wallet-card action-card" onSubmit={onSend}>
      <CardTitle icon={<Send size={18} />} title="Send" />
      <label>
        BOLT-11 invoice
        <textarea
          name="invoice"
          rows={4}
          value={sendInvoice}
          onChange={(event) => setSendInvoice(event.target.value)}
        />
      </label>
      <TextField
        label="Max fee sats"
        name="maxFeeSat"
        type="number"
        min="0"
        step="1"
        value={sendMaxFee}
        onChange={setSendMaxFee}
      />
      <button type="submit" disabled={sendBusy}>
        <Send size={16} />
        Send payment
      </button>
      <output id="send-output">{sendHash}</output>
      <InlineError message={sendError} />
    </form>
  );
}

function ActivityPanel({
  entries,
  logs,
  onRefresh,
}: {
  entries: Entry[];
  logs: LogRow[];
  onRefresh(): void;
}) {
  return (
    <div className="activity-layout">
      <section className="wallet-card activity-card">
        <div className="card-heading compact">
          <CardTitle icon={<Activity size={18} />} title="Swaps" />
          <button id="refresh-swaps" type="button" onClick={onRefresh}>
            <RefreshCw size={16} />
            Refresh swaps
          </button>
        </div>
        <div className="table-frame">
          <table>
            <thead>
              <tr>
                <th>Direction</th>
                <th>State</th>
                <th>Amount</th>
                <th>Payment hash</th>
              </tr>
            </thead>
            <tbody id="swaps-body">
              {entries.map((entry) => (
                <tr key={entry.ID}>
                  <td>{entry.Kind}</td>
                  <td>{entry.Status}</td>
                  <td>{entry.AmountSat}</td>
                  <td>{entry.ID}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      </section>

      <section className="wallet-card log-card">
        <CardTitle icon={<Activity size={18} />} title="Activity" />
        <ol id="activity">
          {logs.map((row) => (
            <li key={`${row.time}-${row.message}`}>
              {row.time} {row.message}
            </li>
          ))}
        </ol>
      </section>
    </div>
  );
}

function CardTitle({ title, icon }: {
  title: string;
  icon: ReactNode;
}) {
  return (
    <div className="card-title">
      {icon}
      <h2>{title}</h2>
    </div>
  );
}

function TextField({
  label,
  name,
  value,
  onChange,
  type = "text",
  min,
  step,
}: {
  label: string;
  name: string;
  value: string;
  onChange(value: string): void;
  type?: string;
  min?: string;
  step?: string;
}) {
  return (
    <label>
      {label}
      <input
        name={name}
        type={type}
        min={min}
        step={step}
        value={value}
        onChange={(event) => onChange(event.target.value)}
      />
    </label>
  );
}

function PasswordField({
  autoComplete,
  value,
  onChange,
}: {
  autoComplete: string;
  value: string;
  onChange(value: string): void;
}) {
  return (
    <label>
      Password
      <input
        name="password"
        type="password"
        autoComplete={autoComplete}
        value={value}
        onChange={(event) => onChange(event.target.value)}
      />
    </label>
  );
}

function CheckField({ label, name, checked, onChange }: {
  label: string;
  name: string;
  checked: boolean;
  onChange(value: boolean): void;
}) {
  return (
    <label className="check">
      <input
        name={name}
        type="checkbox"
        checked={checked}
        onChange={(event) => onChange(event.target.checked)}
      />
      {label}
    </label>
  );
}

function Facts({ id, facts }: {
  id: string;
  facts: Record<string, string | number>;
}) {
  return (
    <dl id={id} className="facts">
      {Object.entries(facts).map(([key, value]) => (
        <div key={key}>
          <dt>{key}</dt>
          <dd>{value}</dd>
        </div>
      ))}
    </dl>
  );
}

function EntryList({ entries }: { entries: Entry[] }) {
  return (
    <ul className="entry-list">
      {entries.map((entry) => (
        <li key={entry.ID}>
          <span>{entry.Kind}</span>
          <strong>{formatSats(entry.AmountSat)}</strong>
          <small>{entry.Status}</small>
        </li>
      ))}
    </ul>
  );
}

function EmptyState({ text }: { text: string }) {
  return <div className="empty-state">{text}</div>;
}

function SummaryPill({ label, value }: {
  label: string;
  value: string;
}) {
  return (
    <div className="summary-pill">
      <span>{label}</span>
      <strong>{value}</strong>
    </div>
  );
}

function InlineError({ message }: { message: string }) {
  if (!message) {
    return null;
  }

  return <p className="inline-error">{message}</p>;
}

function busyOperation(operations: Record<WalletOperation, { busy: boolean }>) {
  return Object.values(operations).some((operation) => operation.busy);
}

function isRuntimeStarted(phase: RuntimePhase) {
  return (
    phase === "needsWallet" ||
    phase === "locked" ||
    phase === "syncing" ||
    phase === "ready"
  );
}

function statusLabel(phase: RuntimePhase) {
  switch (phase) {
  case "loading":
    return "loading wasm";

  case "runtimeReady":
    return "wasm ready";

  case "starting":
    return "starting";

  case "needsWallet":
    return "wallet not created";

  case "locked":
    return "wallet locked";

  case "syncing":
    return "wallet syncing";

  case "ready":
    return "runtime started";

  case "stopping":
    return "stopping";

  case "stopped":
    return "runtime stopped";

  default:
    return "runtime error";
  }
}

function totalBalance(balance: ReturnType<typeof useWalletDK>["balance"]) {
  if (!balance) {
    return 0;
  }

  return Number(balance.TotalConfirmedSat ?? balance.ConfirmedSat ?? 0);
}

function formatSats(value: number) {
  return `${new Intl.NumberFormat().format(value)} sats`;
}

function hostname(value: string) {
  try {
    return new URL(value).hostname;
  } catch {
    return value;
  }
}

function delay(ms: number) {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

function errorMessage(err: unknown): string {
  if (err instanceof Error && err.message) {
    return err.message;
  }
  if (typeof err === "string") {
    return err;
  }

  return JSON.stringify(err);
}
