import { ReactNode, useEffect } from "react";
import { createPortal } from "react-dom";

// Modal renders a dismissible overlay dialog through a portal. It dims the
// page, centers its children in a card, and closes on Escape or a backdrop
// click. A 1 px accent line across the top of the card gives the dialog a
// quiet signature that integrates with the accent color in both themes.
export function Modal({
  open,
  onClose,
  labelledBy,
  children,
}: {
  open: boolean;
  onClose: () => void;
  labelledBy?: string;
  children: ReactNode;
}) {
  useEffect(() => {
    if (!open) {
      return;
    }

    function onKey(e: KeyboardEvent) {
      if (e.key === "Escape") {
        onClose();
      }
    }

    document.addEventListener("keydown", onKey);

    return () => document.removeEventListener("keydown", onKey);
  }, [open, onClose]);

  if (!open) {
    return null;
  }

  return createPortal(
    // Backdrop: full-screen dim + blur, fades in via modal-backdrop animation.
    // Clicking the backdrop (but not the card) dismisses the dialog.
    <div
      className="fixed inset-0 z-50 flex items-center justify-center p-4
        backdrop-blur-sm"
      style={{
        backgroundColor: "rgba(0,0,0,0.55)",
        animation: "var(--animate-modal-backdrop)",
      }}
      onClick={onClose}
    >
      {/* Card: sharp-cornered surface that animates in slightly behind the
          backdrop so the sequence reads — dim first, then card arrives. */}
      <div
        role="dialog"
        aria-modal="true"
        aria-labelledby={labelledBy}
        className="relative w-full max-w-sm border border-border bg-surface
          shadow-2xl"
        style={{ animation: "var(--animate-modal-card)" }}
        onClick={(e) => e.stopPropagation()}
      >
        {/* Accent hairline across the top — a single pixel of --accent drawn as
            an absolute stripe so it sits flush with the border. */}
        <div
          className="absolute inset-x-0 top-0 h-px bg-accent"
          aria-hidden="true"
        />

        {/* Content slot — callers supply their own padding/layout. */}
        <div className="p-6">{children}</div>
      </div>
    </div>,
    document.body,
  );
}
