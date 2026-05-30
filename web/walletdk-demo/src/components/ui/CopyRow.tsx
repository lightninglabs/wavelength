import { CopyButton } from "./CopyButton";

// CopyRow shows a labelled monospace value with an inline copy control.
export function CopyRow({ label, value }: { label: string; value: string }) {
  return (
    <div>
      <div className="mb-1.5 flex items-center justify-between">
        <span className="text-xs font-medium text-muted">{label}</span>
        <CopyButton value={value} />
      </div>
      <div
        className="break-all border border-border bg-well px-3 py-2.5
          font-mono text-xs tabular-nums text-fg"
      >
        {value}
      </div>
    </div>
  );
}
