import { Toggle } from "./Toggle";

// ToggleRow is a labelled switch row used in gateway/security configuration.
export function ToggleRow({
  title,
  subtitle,
  on,
  onChange,
  disabled = false,
}: {
  title: string;
  subtitle: string;
  on: boolean;
  onChange: (next: boolean) => void;
  disabled?: boolean;
}) {
  return (
    <div className="flex items-center justify-between gap-3">
      <div>
        <div className="text-sm font-medium text-fg">{title}</div>
        <div className="text-xs text-muted">{subtitle}</div>
      </div>
      <Toggle on={on} onChange={onChange} ariaLabel={title} disabled={disabled} />
    </div>
  );
}
