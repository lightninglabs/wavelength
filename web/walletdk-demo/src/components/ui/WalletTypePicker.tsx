import { Fingerprint, KeyRound, type LucideIcon } from "lucide-react";
import { cn } from "../../lib/cn";

export type WalletMode = "passkey" | "password";

type WalletTypeOption = {
  value: WalletMode;
  label: string;
  description: string;
  icon: LucideIcon;
};

const OPTIONS: ReadonlyArray<WalletTypeOption> = [
  {
    value: "passkey",
    label: "Passkey",
    description:
      "Unlock with Face ID, Touch ID, or your device PIN. Syncs across devices.",
    icon: Fingerprint,
  },
  {
    value: "password",
    label: "Password",
    description:
      "Choose a password you remember. Works everywhere, including older browsers.",
    icon: KeyRound,
  },
];

// WalletTypePicker is a prominent two-option selector for passkey vs password
// wallet creation. Card layout makes the choice obvious before the user fills
// the form below.
export function WalletTypePicker({
  value,
  onChange,
}: {
  value: WalletMode;
  onChange: (next: WalletMode) => void;
}) {
  return (
    <div
      role="radiogroup"
      aria-label="Wallet type"
      className="grid gap-3 sm:grid-cols-2"
    >
      {OPTIONS.map((option) => {
        const selected = value === option.value;
        const Icon = option.icon;

        return (
          <button
            key={option.value}
            type="button"
            role="radio"
            aria-checked={selected}
            onClick={() => onChange(option.value)}
            className={cn(
              `group relative flex flex-col items-start gap-3 border p-4
              text-left transition-all duration-200`,
              selected
                ? "border-accent bg-accent-soft shadow-[inset_0_0_0_1px_var(--accent)]"
                : "border-border bg-surface-alt hover:border-border-strong hover:bg-surface",
            )}
          >
            <div
              className={cn(
                "flex h-10 w-10 items-center justify-center border transition-colors",
                selected
                  ? "border-accent/30 bg-surface text-accent"
                  : "border-border bg-well text-muted group-hover:text-fg",
              )}
            >
              <Icon size={20} strokeWidth={1.75} />
            </div>

            <div className="min-w-0 space-y-1">
              <div
                className={cn(
                  "text-sm font-semibold tracking-tight",
                  selected ? "text-fg" : "text-fg/90",
                )}
              >
                {option.label}
              </div>
              <p className="text-xs leading-relaxed text-muted">
                {option.description}
              </p>
            </div>

            <span
              aria-hidden
              className={cn(
                "absolute right-3 top-3 h-2.5 w-2.5 rounded-full border-2 transition-colors",
                selected
                  ? "border-accent bg-accent"
                  : "border-border-strong bg-transparent",
              )}
            />
          </button>
        );
      })}
    </div>
  );
}
