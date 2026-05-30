import { cn } from "../../lib/cn";

// Toggle is the Zones square switch: a sharp-cornered 2:1 track with a square
// knob, matching the squared, recessed language of the rest of the UI.
export function Toggle({
  on,
  onChange,
  ariaLabel,
  disabled = false,
}: {
  on: boolean;
  onChange: (next: boolean) => void;
  ariaLabel?: string;
  disabled?: boolean;
}) {
  return (
    <button
      type="button"
      onClick={() => onChange(!on)}
      disabled={disabled}
      aria-pressed={on}
      aria-label={ariaLabel}
      className={cn(
        "relative block h-[22px] w-11 shrink-0 transition-colors",
        on ? "bg-accent" : "bg-border-strong",
        disabled && "cursor-not-allowed opacity-60",
      )}
    >
      <span
        className={cn(
          "absolute left-[3px] top-[3px] h-4 w-4 bg-white transition-transform",
          on ? "translate-x-[22px]" : "translate-x-0",
        )}
      />
    </button>
  );
}
