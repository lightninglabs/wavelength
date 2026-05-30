import { useState } from "react";
import { Check, Copy } from "lucide-react";
import { cn } from "../../lib/cn";

// CopyButton writes a value to the clipboard and shows a transient "Copied"
// confirmation.
export function CopyButton({
  value,
  label = "Copy",
}: {
  value: string;
  label?: string;
}) {
  const [done, setDone] = useState(false);

  return (
    <button
      type="button"
      onClick={() => {
        try {
          void navigator.clipboard?.writeText(value);
        } catch {
          // Clipboard may be unavailable; the confirmation is best-effort.
        }
        setDone(true);
        window.setTimeout(() => setDone(false), 1400);
      }}
      className={cn(
        `inline-flex items-center gap-1.5 border border-border px-2 py-1
        text-xs font-medium transition-colors`,
        done ? "text-good" : "text-muted",
      )}
    >
      {done ? <Check size={13} /> : <Copy size={13} />}
      {done ? "Copied" : label}
    </button>
  );
}
