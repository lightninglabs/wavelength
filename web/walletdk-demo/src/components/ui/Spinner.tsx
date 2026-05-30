import { Loader } from "lucide-react";
import { cn } from "../../lib/cn";

// Spinner is a spinning loader icon tinted with the accent colour.
export function Spinner({
  size = 18,
  className,
}: {
  size?: number;
  className?: string;
}) {
  return <Loader size={size} className={cn("animate-spin text-accent", className)} />;
}
