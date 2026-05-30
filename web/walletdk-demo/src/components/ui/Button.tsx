import { ReactNode } from "react";
import { Loader, type LucideIcon } from "lucide-react";
import { cn } from "../../lib/cn";

type ButtonProps = {
  children: ReactNode;
  onClick?: () => void;
  icon?: LucideIcon;
  type?: "button" | "submit";
  disabled?: boolean;
  busy?: boolean;
  // block defaults to true (full-width). Pass false for an inline, content-width
  // button, e.g. an action sitting inside a wide Band.
  block?: boolean;
  className?: string;
};

// ButtonIcon renders the leading glyph: a spinning loader while busy, otherwise
// the supplied icon (if any). It keeps a fixed 16px box so the label never
// shifts as the button toggles between idle and busy.
function ButtonIcon({ icon: Icon, busy }: { icon?: LucideIcon; busy?: boolean }) {
  if (busy) {
    return <Loader size={16} className="animate-spin" />;
  }

  return Icon ? <Icon size={16} /> : null;
}

// PrimaryButton is the single accent-filled call to action. When `busy` it
// shows a spinner and is non-interactive without changing its label.
export function PrimaryButton({
  children,
  onClick,
  icon,
  type = "button",
  disabled = false,
  busy = false,
  block = true,
  className,
}: ButtonProps) {
  return (
    <button
      type={type}
      onClick={onClick}
      disabled={disabled || busy}
      className={cn(
        `inline-flex items-center justify-center gap-2 bg-accent px-4 py-2.5
        text-sm font-semibold text-white transition-opacity hover:opacity-90
        disabled:opacity-50`,
        block && "w-full",
        className,
      )}
    >
      <ButtonIcon icon={icon} busy={busy} />
      {children}
    </button>
  );
}

// GhostButton is a hairline-bordered secondary action.
export function GhostButton({
  children,
  onClick,
  icon,
  type = "button",
  disabled = false,
  busy = false,
  block = true,
  className,
}: ButtonProps) {
  return (
    <button
      type={type}
      onClick={onClick}
      disabled={disabled || busy}
      className={cn(
        `inline-flex items-center justify-center gap-2 border border-border
        bg-surface-alt px-4 py-2.5 text-sm font-medium text-fg transition-colors
        hover:border-border-strong disabled:opacity-50`,
        block && "w-full",
        className,
      )}
    >
      <ButtonIcon icon={icon} busy={busy} />
      {children}
    </button>
  );
}

// TextLink is the small accent anchor used to cross-link auth flows.
export function TextLink({
  children,
  onClick,
}: {
  children: ReactNode;
  onClick?: () => void;
}) {
  return (
    <button type="button" onClick={onClick} className="font-medium text-accent">
      {children}
    </button>
  );
}
