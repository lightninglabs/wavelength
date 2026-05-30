import { TriangleAlert } from "lucide-react";

// InlineError renders a form-level error message, or nothing when empty.
export function InlineError({ message }: { message: string }) {
  if (!message) {
    return null;
  }

  return (
    <p className="flex items-start gap-2 text-sm text-bad">
      <TriangleAlert size={15} className="mt-0.5 shrink-0" />
      <span className="break-words">{message}</span>
    </p>
  );
}
