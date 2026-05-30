import { useState } from "react";
import { Fingerprint, KeyRound, ShieldCheck } from "lucide-react";
import { AuthHeader } from "../../components/layout/AuthHeader";
import { AuthLayout } from "../../components/layout/AuthLayout";
import { Field } from "../../components/ui/Field";
import { InlineError } from "../../components/ui/InlineError";
import { PrimaryButton } from "../../components/ui/Button";
import { Segmented } from "../../components/ui/Segmented";
import { Toggle } from "../../components/ui/Toggle";

type Mode = "create" | "restore";

// resize grows or shrinks a word list to the requested length, preserving any
// already-entered words.
function resize(words: string[], length: number): string[] {
  const next = words.slice(0, length);
  while (next.length < length) {
    next.push("");
  }

  return next;
}

// parseMnemonicPaste splits clipboard text on whitespace (spaces, tabs,
// newlines) into individual recovery words.
function parseMnemonicPaste(text: string): string[] {
  return text
    .trim()
    .split(/\s+/)
    .map((w) => w.trim())
    .filter((w) => w.length > 0);
}

// CreateScreen serves the `needsWallet` phase: the runtime started but no wallet
// exists yet. The user either creates a fresh wallet (generating a recovery
// phrase shown next on the backup screen) or restores one from an existing
// phrase. Both paths call createWallet under the hood.
export function CreateScreen({
  network,
  passkeySupported,
  onCreate,
  onRestore,
  busy,
  error,
}: {
  network: string;
  passkeySupported: boolean;
  onCreate: (args: { password: string; enablePasskey: boolean }) => void;
  onRestore: (args: {
    password: string;
    mnemonic: string[];
    passphrase: string;
  }) => void;
  busy: boolean;
  error: string;
}) {
  const [mode, setMode] = useState<Mode>("create");
  const [password, setPassword] = useState("");
  const [confirm, setConfirm] = useState("");
  const [enablePasskey, setEnablePasskey] = useState(passkeySupported);
  const [count, setCount] = useState<12 | 24>(12);
  const [words, setWords] = useState<string[]>(() => resize([], 12));
  const [passphrase, setPassphrase] = useState("");

  const passwordOk = password.length > 0 && password === confirm;
  const wordsOk = words.every((w) => w.trim().length > 0);
  const canSubmit =
    !busy && passwordOk && (mode === "create" || wordsOk);

  // handleWordPaste distributes a multi-word clipboard string across the
  // recovery phrase inputs (e.g. paste all 24 words at once).
  function handleWordPaste(e: React.ClipboardEvent<HTMLInputElement>) {
    const parts = parseMnemonicPaste(e.clipboardData.getData("text"));
    if (parts.length <= 1) {
      return;
    }

    e.preventDefault();

    const length: 12 | 24 =
      parts.length === 12 ? 12 : parts.length === 24 ? 24 : count;

    if (length !== count) {
      setCount(length);
    }
    setWords(resize(parts, length));
  }

  function submit() {
    if (!canSubmit) {
      return;
    }

    if (mode === "create") {
      onCreate({ password, enablePasskey });

      return;
    }

    onRestore({
      password,
      mnemonic: words.map((w) => w.trim()),
      passphrase: passphrase.trim(),
    });
  }

  return (
    <AuthLayout network={network} wide={mode === "restore"}>
      <AuthHeader
        title={mode === "create" ? "Create wallet" : "Restore wallet"}
        sub={
          mode === "create"
            ? "Keys are generated and stored on this device."
            : "Enter your recovery phrase to rebuild this wallet on-device."
        }
      />
      <div className="mb-5">
        <Segmented
          value={mode}
          onChange={(m) => setMode(m)}
          options={[
            { value: "create", label: "New wallet" },
            { value: "restore", label: "Restore" },
          ]}
        />
      </div>

      <form
        className="space-y-4"
        onSubmit={(e) => {
          e.preventDefault();
          submit();
        }}
      >
        <Field
          label={mode === "create" ? "Password" : "New password"}
          type="password"
          placeholder="••••••••••"
          value={password}
          onChange={setPassword}
        />
        <Field
          label="Confirm password"
          type="password"
          placeholder="••••••••••"
          value={confirm}
          onChange={setConfirm}
        />

        {mode === "create" && passkeySupported ? (
          <div
            className="flex items-center justify-between gap-3 border
              border-border bg-surface-alt px-4 py-3"
          >
            <div className="flex min-w-0 items-center gap-2.5">
              <Fingerprint size={18} className="text-accent" />
              <div className="min-w-0">
                <div className="text-sm font-medium text-fg">
                  Enable passkey
                </div>
                <div className="truncate text-xs text-muted">
                  Unlock with Face ID / Touch ID
                </div>
              </div>
            </div>
            <Toggle
              on={enablePasskey}
              onChange={setEnablePasskey}
              ariaLabel="Enable passkey"
            />
          </div>
        ) : null}

        {mode === "restore" ? (
          <>
            <div>
              <div className="mb-2 flex items-center justify-between">
                <span className="text-[10px] font-semibold uppercase tracking-[0.16em] text-muted">
                  Recovery phrase
                </span>
                <Segmented
                  size="sm"
                  value={String(count)}
                  onChange={(v) => {
                    const n = Number(v) as 12 | 24;
                    setCount(n);
                    setWords((w) => resize(w, n));
                  }}
                  options={[
                    { value: "12", label: "12 words" },
                    { value: "24", label: "24 words" },
                  ]}
                />
              </div>
              <div className="grid grid-cols-2 gap-2 sm:grid-cols-3">
                {words.map((word, i) => (
                  <div
                    key={i}
                    className="flex items-center gap-2 border border-border
                      bg-well px-2.5 py-1.5"
                  >
                    <span className="font-mono text-xs tabular-nums text-faint">
                      {String(i + 1).padStart(2, "0")}
                    </span>
                    <input
                      className="w-full bg-transparent text-sm text-fg
                        outline-none"
                      aria-label={`Word ${i + 1}`}
                      value={word}
                      onChange={(e) =>
                        setWords((w) =>
                          w.map((x, idx) => (idx === i ? e.target.value : x)),
                        )
                      }
                      onPaste={handleWordPaste}
                    />
                  </div>
                ))}
              </div>
            </div>

            <Field
              label="BIP-39 passphrase (optional)"
              type="password"
              placeholder="leave blank if unused"
              value={passphrase}
              onChange={setPassphrase}
            />
          </>
        ) : null}

        <PrimaryButton type="submit" icon={KeyRound} disabled={!canSubmit}>
          {mode === "create"
            ? busy
              ? "Creating wallet…"
              : "Create wallet"
            : busy
              ? "Restoring wallet…"
              : "Restore wallet"}
        </PrimaryButton>
        <InlineError message={error} />

        {mode === "create" ? (
          <div className="flex items-center gap-2 text-xs text-faint">
            <ShieldCheck size={13} className="text-good" />
            On-device keys · nothing leaves this browser.
          </div>
        ) : null}
      </form>
    </AuthLayout>
  );
}
