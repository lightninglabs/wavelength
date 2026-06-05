import { useState } from "react";
import { KeyRound } from "lucide-react";
import { AuthHeader } from "../../components/layout/AuthHeader";
import { AuthLayout } from "../../components/layout/AuthLayout";
import { Field } from "../../components/ui/Field";
import { InlineError } from "../../components/ui/InlineError";
import {
  GhostButton,
  PrimaryButton,
} from "../../components/ui/Button";
import { Segmented } from "../../components/ui/Segmented";

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

// RestoreWalletScreen rebuilds a wallet on-device from an existing recovery
// phrase. Restores are always password wallets, so it collects a new local
// password alongside the phrase.
export function RestoreWalletScreen({
  network,
  onRestore,
  onBack,
  busy,
  error,
}: {
  network: string;
  onRestore: (args: {
    password: string;
    mnemonic: string[];
    passphrase: string;
  }) => void;
  onBack: () => void;
  busy: boolean;
  error: string;
}) {
  const [password, setPassword] = useState("");
  const [confirm, setConfirm] = useState("");
  const [count, setCount] = useState<12 | 24>(12);
  const [words, setWords] = useState<string[]>(() => resize([], 12));
  const [passphrase, setPassphrase] = useState("");

  const passwordOk = password.length > 0 && password === confirm;
  const wordsOk = words.every((w) => w.trim().length > 0);
  const canSubmit = !busy && passwordOk && wordsOk;

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

  // submit validates canSubmit and calls onRestore with trimmed words and
  // passphrase.
  function submit() {
    if (!canSubmit) {
      return;
    }

    onRestore({
      password,
      mnemonic: words.map((w) => w.trim()),
      passphrase: passphrase.trim(),
    });
  }

  return (
    <AuthLayout network={network} wide>
      <AuthHeader
        title="Restore wallet"
        sub="Enter your recovery phrase to rebuild this wallet on-device."
      />
      <form
        className="space-y-4"
        onSubmit={(e) => {
          e.preventDefault();
          submit();
        }}
      >
        <Field
          label="New password"
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

        <PrimaryButton type="submit" icon={KeyRound} disabled={!canSubmit}>
          {busy ? "Restoring wallet…" : "Restore wallet"}
        </PrimaryButton>
        <InlineError message={error} />
        <GhostButton onClick={onBack} disabled={busy}>
          Back
        </GhostButton>
      </form>
    </AuthLayout>
  );
}
