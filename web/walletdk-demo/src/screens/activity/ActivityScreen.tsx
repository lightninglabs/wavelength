import { RefreshCw } from "lucide-react";
import { Entry } from "@lightninglabs/walletdk-core";
import { ActivityRow } from "../../components/ActivityRow";
import { PageHead } from "../../components/layout/PageHead";
import { AppTab } from "../../components/layout/nav";
import { Band } from "../../components/ui/Band";
import { Label } from "../../components/ui/Label";
import { cn } from "../../lib/cn";
import { dayLabel } from "../../lib/format";

// groupByDay buckets entries into ordered day groups, preserving the incoming
// (newest-first) order within and across groups.
function groupByDay(entries: Entry[]): Array<{ day: string; items: Entry[] }> {
  const groups: Array<{ day: string; items: Entry[] }> = [];

  for (const entry of entries) {
    const day = dayLabel(entry.CreatedAt);
    const last = groups[groups.length - 1];
    if (last && last.day === day) {
      last.items.push(entry);
    } else {
      groups.push({ day, items: [entry] });
    }
  }

  return groups;
}

// ActivityScreen lists the full transaction history grouped by day, one band per
// day with the rows divided by hairlines.
export function ActivityScreen({
  activity,
  onNavigate,
  onRefresh,
  busy,
}: {
  activity: Entry[];
  onNavigate: (tab: AppTab) => void;
  onRefresh: () => void;
  busy: boolean;
}) {
  const groups = groupByDay(activity);

  return (
    <div>
      <PageHead
        title="Activity"
        subtitle="Complete payment history"
        onBack={() => onNavigate("home")}
        trailing={
          <button
            type="button"
            onClick={onRefresh}
            disabled={busy}
            className="inline-flex items-center gap-1.5 border border-border
              px-3 py-2 text-xs font-medium text-muted transition-colors
              hover:text-fg disabled:opacity-50"
          >
            <RefreshCw size={14} className={cn(busy && "animate-spin")} />
            Refresh
          </button>
        }
      />
      {groups.length === 0 ? (
        <Band>
          <div className="py-6 text-center text-sm text-muted">
            No activity yet.
          </div>
        </Band>
      ) : (
        groups.map((group, gi) => (
          <Band key={`${group.day}-${gi}`} tinted={gi % 2 === 0}>
            <Label>{group.day}</Label>
            <div className="mt-2 divide-y divide-border border-t border-border">
              {group.items.map((entry) => (
                <ActivityRow key={entry.ID} entry={entry} />
              ))}
            </div>
          </Band>
        ))
      )}
    </div>
  );
}
