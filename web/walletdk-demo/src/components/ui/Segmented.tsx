import { cn } from "../../lib/cn";

// Segmented is a single-select pill group used for network, create/restore mode
// and word-count toggles.
export function Segmented<T extends string>({
  value,
  options,
  onChange,
  size = "md",
}: {
  value: T;
  options: ReadonlyArray<{ value: T; label: string }>;
  onChange: (next: T) => void;
  size?: "sm" | "md";
}) {
  return (
    <div className="inline-flex border border-border">
      {options.map((o, i) => {
        const on = value === o.value;

        return (
          <button
            key={o.value}
            type="button"
            onClick={() => onChange(o.value)}
            className={cn(
              "whitespace-nowrap font-medium capitalize transition-colors",
              i > 0 && "border-l border-border",
              size === "sm" ? "px-3 py-1 text-xs" : "px-3.5 py-1.5 text-sm",
              on ? "bg-well text-fg" : "text-muted hover:text-fg",
            )}
          >
            {o.label}
          </button>
        );
      })}
    </div>
  );
}
