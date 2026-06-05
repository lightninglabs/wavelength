import { Modal } from "./Modal";
import { GhostButton } from "./Button";
import { cn } from "../../lib/cn";

// ConfirmDialog asks the user to confirm or cancel an action inside a Modal.
// When `destructive` it renders the confirm action with the danger treatment
// (filled bg-bad) rather than the outlined variant used in the settings danger
// zone — the filled treatment gives decisive weight when the confirm action
// itself is the hazard.
export function ConfirmDialog({
  open,
  title,
  description,
  confirmLabel,
  cancelLabel = "Cancel",
  destructive = false,
  busy = false,
  onConfirm,
  onCancel,
}: {
  open: boolean;
  title: string;
  description: string;
  confirmLabel: string;
  cancelLabel?: string;
  destructive?: boolean;
  busy?: boolean;
  onConfirm: () => void;
  onCancel: () => void;
}) {
  return (
    <Modal open={open} onClose={onCancel} labelledBy="confirm-dialog-title">
      <h2
        id="confirm-dialog-title"
        className="text-base font-semibold text-fg"
      >
        {title}
      </h2>
      <p className="mt-2 text-sm leading-relaxed text-muted">{description}</p>
      <div className="mt-6 grid grid-cols-2 gap-3">
        <GhostButton onClick={onCancel} disabled={busy}>
          {cancelLabel}
        </GhostButton>
        <button
          type="button"
          onClick={onConfirm}
          disabled={busy}
          className={cn(
            `inline-flex items-center justify-center gap-2 px-4 py-2.5 text-sm
            font-semibold text-white transition-opacity hover:opacity-90
            disabled:opacity-50`,
            destructive ? "bg-bad" : "bg-accent",
          )}
        >
          {confirmLabel}
        </button>
      </div>
    </Modal>
  );
}
