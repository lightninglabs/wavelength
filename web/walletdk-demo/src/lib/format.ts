// Pure formatting helpers shared across screens. No JSX, no SDK calls.

const satFormatter = new Intl.NumberFormat("en-US");

// formatSats renders a sat value with thousands separators.
export function formatSats(value: number): string {
  return satFormatter.format(Math.round(value));
}

// formatBtc renders a sat value as a BTC amount with 8 decimals.
export function formatBtc(value: number): string {
  return (value / 100_000_000).toFixed(8);
}

// pct returns part/total as a 0-100 number, guarding against divide-by-zero.
export function pct(part: number, total: number): number {
  if (total <= 0) {
    return 0;
  }

  return (part / total) * 100;
}

// shortKey truncates a long hex string / address / invoice for compact display.
export function shortKey(value: string, head = 6, tail = 6): string {
  if (!value || value.length <= head + tail + 1) {
    return value;
  }

  return `${value.slice(0, head)}…${value.slice(-tail)}`;
}

// formatTimestamp renders an ISO timestamp (Entry.CreatedAt) as a short local
// time, falling back to the raw value when it cannot be parsed.
export function formatTimestamp(iso?: string): string {
  if (!iso) {
    return "";
  }

  const date = new Date(iso);
  if (Number.isNaN(date.getTime())) {
    return iso;
  }

  return date.toLocaleString([], {
    month: "short",
    day: "numeric",
    hour: "numeric",
    minute: "2-digit",
  });
}

// dayLabel buckets an ISO timestamp into a human day heading (Today / Yesterday
// / a short date), used to group the activity history.
export function dayLabel(iso?: string): string {
  if (!iso) {
    return "Earlier";
  }

  const date = new Date(iso);
  if (Number.isNaN(date.getTime())) {
    return "Earlier";
  }

  const startOfDay = (d: Date) =>
    new Date(d.getFullYear(), d.getMonth(), d.getDate()).getTime();
  const dayMs = 86_400_000;
  const diffDays = Math.round((startOfDay(new Date()) - startOfDay(date)) / dayMs);

  if (diffDays === 0) {
    return "Today";
  }
  if (diffDays === 1) {
    return "Yesterday";
  }

  return date.toLocaleDateString([], { month: "short", day: "numeric" });
}
