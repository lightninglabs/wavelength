import { cn } from "../../lib/cn";

// Field is a labelled text/password input matched to the dark/light surface.
// When `onChange` is provided it is controlled; otherwise it is uncontrolled
// with an optional `defaultValue`.
export function Field({
  label,
  type = "text",
  placeholder,
  defaultValue,
  value,
  onChange,
  mono = false,
  inputMode,
  disabled = false,
}: {
  label: string;
  type?: string;
  placeholder?: string;
  defaultValue?: string;
  value?: string;
  onChange?: (next: string) => void;
  mono?: boolean;
  inputMode?: "numeric" | "text";
  disabled?: boolean;
}) {
  return (
    <label className="block">
      <span className="mb-2 block text-[10px] font-semibold uppercase tracking-[0.16em] text-muted">
        {label}
      </span>
      <input
        type={type}
        inputMode={inputMode}
        placeholder={placeholder}
        defaultValue={onChange ? undefined : defaultValue}
        value={value}
        disabled={disabled}
        onChange={onChange ? (e) => onChange(e.target.value) : undefined}
        className={cn(
          `w-full border border-border bg-well px-3 py-2.5 text-sm text-fg
          outline-none transition-colors focus:border-border-strong
          disabled:cursor-not-allowed disabled:text-muted`,
          mono && "font-mono tabular-nums",
        )}
      />
    </label>
  );
}
