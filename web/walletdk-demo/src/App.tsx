import {
  Activity,
  CircleStop,
  Copy,
  Download,
  KeyRound,
  LoaderCircle,
  Play,
  Plus,
  RefreshCw,
  Send,
  Wallet,
} from "lucide-react";
import {
  FormEvent,
  ReactNode,
  useEffect,
  useMemo,
  useState,
} from "react";
import { RuntimeConfig } from "@lightninglabs/walletdk-core";
import { useWalletDK } from "@lightninglabs/walletdk-react";

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

export function App() {
  const wallet = useWalletDK();
  const [runtimeForm, setRuntimeForm] = useState<RuntimeForm>(signetDefaults);
  const [logs, setLogs] = useState<LogRow[]>([]);
  const [password, setPassword] = useState("");
  const [mnemonic, setMnemonic] = useState<string[]>([]);
  const [mnemonicAcknowledged, setMnemonicAcknowledged] = useState(false);
  const [address, setAddress] = useState("");
  const [receiveAmount, setReceiveAmount] = useState("1000");
  const [receiveMemo, setReceiveMemo] = useState("walletdk demo receive");
  const [invoice, setInvoice] = useState("");
  const [sendInvoice, setSendInvoice] = useState("");
  const [sendMaxFee, setSendMaxFee] = useState("0");
  const [sendHash, setSendHash] = useState("");
  const [busy, setBusy] = useState("");

  const runtimeStarted = wallet.phase === "started";
  const walletReady = Boolean(wallet.info?.WalletReady);
  const needsBootstrap = runtimeStarted && !walletReady;
  const showDashboard = runtimeStarted && walletReady && mnemonicAcknowledged;
  const statusText = statusLabel(wallet.phase);

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
      "total confirmed": balance.TotalConfirmedSat ?? balance.ConfirmedSat ??
        "",
      "on-chain wallet": balance.OnchainWalletConfirmedSat ??
        balance.ConfirmedSat ?? "",
    };
  }, [wallet.balance]);

  async function guarded(label: string, fn: () => Promise<void>) {
    try {
      setBusy(label);
      await fn();
    } catch (err) {
      log(errorMessage(err));
    } finally {
      setBusy("");
    }
  }

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

    await guarded("start", async () => {
      const info = await wallet.start(runtimeForm);
      setMnemonicAcknowledged(Boolean(info.WalletReady));
      log("runtime started");
    });
  }

  async function createWallet(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();

    await guarded("create", async () => {
      const result = await wallet.client.createWallet({ password });
      setMnemonic(result.Mnemonic || []);
      setMnemonicAcknowledged(false);
      log(`wallet created ${result.IdentityPubKey}`);
    });
  }

  async function unlockWallet(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();

    await guarded("unlock", async () => {
      const result = await wallet.client.unlockWallet({ password });
      setMnemonicAcknowledged(true);
      log(`wallet unlocked ${result.IdentityPubKey}`);
      await waitForWalletReady();
      await wallet.refresh();
    });
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
    await guarded("refresh", async () => {
      await wallet.refresh();
    });
  }

  async function stopRuntime() {
    await guarded("stop", async () => {
      await wallet.stop();
      setMnemonic([]);
      setMnemonicAcknowledged(false);
      setAddress("");
      setInvoice("");
      setSendHash("");
      log("runtime stopped");
    });
  }

  async function createAddress(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();

    await guarded("deposit", async () => {
      const result = await wallet.client.deposit();
      setAddress(result.Address);
      log("created onboarding address");
      await wallet.refresh();
    });
  }

  async function receive(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();

    await guarded("receive", async () => {
      const result = await wallet.client.receive({
        amountSat: Number(receiveAmount),
        memo: receiveMemo,
      });
      setInvoice(result.Invoice);
      log(`receive swap ${result.PaymentHash || result.Entry?.ID || ""}`);
      await wallet.refresh();
    });
  }

  async function send(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();

    await guarded("send", async () => {
      const result = await wallet.client.send({
        invoice: sendInvoice,
        maxFeeSat: Number(sendMaxFee),
      });
      const paymentHash = result.PaymentHash || result.Entry?.ID || "";
      setSendHash(paymentHash);
      log(`send swap ${paymentHash}`);
      await wallet.refresh();
    });
  }

  return (
    <>
      <header className="topbar">
        <div>
          <h1>walletdk demo</h1>
          <p>OPFS-backed browser wallet runtime for walletdk.</p>
        </div>
        <div id="runtime-status" className={`status status-${wallet.phase}`}>
          {busy ? <LoaderCircle size={15} className="spin" /> : null}
          {statusText}
        </div>
      </header>

      <main>
        <section id="setup-view" className="view" hidden={showDashboard}>
          <form id="runtime-form" className="panel" onSubmit={startRuntime}>
            <PanelHeader icon={<Wallet size={18} />} title="Runtime" />
            <div className="grid">
              <TextField
                label="Network"
                name="network"
                value={runtimeForm.network}
                onChange={(value) => updateRuntime("network", value)}
              />
              <TextField
                label="Data dir"
                name="dataDir"
                value={runtimeForm.dataDir}
                onChange={(value) => updateRuntime("dataDir", value)}
              />
              <TextField
                label="Ark gateway URL"
                name="arkGatewayURL"
                value={runtimeForm.arkGatewayURL}
                onChange={(value) => updateRuntime("arkGatewayURL", value)}
              />
              <TextField
                label="Mailbox gateway URL"
                name="mailboxGatewayURL"
                value={runtimeForm.mailboxGatewayURL}
                onChange={(value) => updateRuntime("mailboxGatewayURL", value)}
              />
              <TextField
                label="Esplora URL"
                name="walletEsploraURL"
                value={runtimeForm.walletEsploraURL}
                onChange={(value) => updateRuntime("walletEsploraURL", value)}
              />
              <TextField
                label="Swap server gateway URL"
                name="swapServerGatewayURL"
                value={runtimeForm.swapServerGatewayURL}
                onChange={(value) => updateRuntime("swapServerGatewayURL", value)}
              />
              <TextField
                label="Swap mailbox gateway URL"
                name="swapMailboxGatewayURL"
                value={runtimeForm.swapMailboxGatewayURL}
                onChange={(value) => {
                  updateRuntime("swapMailboxGatewayURL", value);
                }}
              />
              <TextField
                label="Swap DB"
                name="swapDatabaseFileName"
                value={runtimeForm.swapDatabaseFileName}
                onChange={(value) => {
                  updateRuntime("swapDatabaseFileName", value);
                }}
              />
            </div>
            <div className="inline">
              <CheckField
                label="Insecure Ark transport"
                name="serverInsecure"
                checked={runtimeForm.serverInsecure}
                onChange={(value) => updateRuntime("serverInsecure", value)}
              />
              <CheckField
                label="Insecure swap transport"
                name="swapServerInsecure"
                checked={runtimeForm.swapServerInsecure}
                onChange={(value) => {
                  updateRuntime("swapServerInsecure", value);
                }}
              />
              <CheckField
                label="Wallet only"
                name="disableSwaps"
                checked={runtimeForm.disableSwaps}
                onChange={(value) => updateRuntime("disableSwaps", value)}
              />
            </div>
            <button type="submit" disabled={busy === "start"}>
              <Play size={16} />
              Start runtime
            </button>
          </form>

          <div className="split">
            <form
              id="create-form"
              className="panel"
              hidden={!needsBootstrap}
              onSubmit={createWallet}
            >
              <PanelHeader icon={<Plus size={18} />} title="Create wallet" />
              <PasswordField
                autoComplete="new-password"
                value={password}
                onChange={setPassword}
              />
              <button type="submit" disabled={busy === "create"}>
                <KeyRound size={16} />
                Create wallet
              </button>
            </form>

            <form
              id="unlock-form"
              className="panel"
              hidden={!needsBootstrap}
              onSubmit={unlockWallet}
            >
              <PanelHeader icon={<KeyRound size={18} />} title="Unlock wallet" />
              <PasswordField
                autoComplete="current-password"
                value={password}
                onChange={setPassword}
              />
              <button type="submit" disabled={busy === "unlock"}>
                <KeyRound size={16} />
                Unlock wallet
              </button>
            </form>
          </div>

          <section
            id="mnemonic-panel"
            className="panel warning"
            hidden={mnemonic.length === 0 || mnemonicAcknowledged}
          >
            <PanelHeader icon={<KeyRound size={18} />} title="Mnemonic backup" />
            <p id="mnemonic">{mnemonic.join(" ")}</p>
            <button
              id="mnemonic-ack"
              type="button"
              onClick={async () => {
                setMnemonicAcknowledged(true);
                await wallet.refresh().catch((err) => log(errorMessage(err)));
              }}
            >
              <Copy size={16} />
              I recorded the demo mnemonic
            </button>
          </section>
        </section>

        <section id="dashboard-view" className="view" hidden={!showDashboard}>
          <nav className="actions">
            <button id="refresh" type="button" onClick={refreshAll}>
              <RefreshCw size={16} />
              Refresh
            </button>
            <button id="stop" type="button" onClick={stopRuntime}>
              <CircleStop size={16} />
              Stop runtime
            </button>
          </nav>

          <section className="metrics">
            <Panel title="Overview" icon={<Activity size={18} />}>
              <Facts id="info-grid" facts={facts} />
            </Panel>

            <Panel title="Balance" icon={<Wallet size={18} />}>
              <Facts id="balance-grid" facts={balanceFacts} />
            </Panel>
          </section>

          <section className="split">
            <form id="address-form" className="panel" onSubmit={createAddress}>
              <PanelHeader icon={<Download size={18} />} title="Onboarding address" />
              <button type="submit" disabled={busy === "deposit"}>
                <Plus size={16} />
                New address
              </button>
              <output id="address-output">{address}</output>
            </form>

            <form id="receive-form" className="panel" onSubmit={receive}>
              <PanelHeader icon={<Download size={18} />} title="Receive" />
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
              <button type="submit" disabled={busy === "receive"}>
                <Plus size={16} />
                Create invoice
              </button>
              <output id="receive-output">{invoice}</output>
            </form>
          </section>

          <section className="split">
            <form id="send-form" className="panel" onSubmit={send}>
              <PanelHeader icon={<Send size={18} />} title="Send" />
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
              <button type="submit" disabled={busy === "send"}>
                <Send size={16} />
                Send payment
              </button>
              <output id="send-output">{sendHash}</output>
            </form>

            <section className="panel">
              <PanelHeader icon={<Activity size={18} />} title="Swaps" />
              <button id="refresh-swaps" type="button" onClick={refreshAll}>
                <RefreshCw size={16} />
                Refresh swaps
              </button>
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
                    {wallet.activity.map((entry) => (
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
          </section>

          <section className="panel">
            <PanelHeader icon={<Activity size={18} />} title="Activity" />
            <ol id="activity">
              {logs.map((row) => (
                <li key={`${row.time}-${row.message}`}>
                  {row.time} {row.message}
                </li>
              ))}
            </ol>
          </section>
        </section>
      </main>
    </>
  );
}

function Panel({ title, icon, children }: {
  title: string;
  icon: ReactNode;
  children: ReactNode;
}) {
  return (
    <section className="panel">
      <PanelHeader title={title} icon={icon} />
      {children}
    </section>
  );
}

function PanelHeader({ title, icon }: {
  title: string;
  icon: ReactNode;
}) {
  return (
    <div className="panel-title">
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

function statusLabel(phase: string) {
  switch (phase) {
  case "loading":
    return "loading wasm";

  case "wasm-ready":
    return "wasm ready";

  case "starting":
    return "starting";

  case "started":
    return "runtime started";

  case "stopped":
    return "runtime stopped";

  default:
    return "runtime error";
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
