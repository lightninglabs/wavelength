import {
  ArrowLeftRight,
  Bitcoin,
  Lock,
  type LucideIcon,
  ShieldCheck,
  Wallet,
} from "lucide-react";

const BULLETS: Array<{ icon: LucideIcon; title: string; sub: string }> = [
  {
    icon: ShieldCheck,
    title: "Keys on this device",
    sub: "They are generated and stored locally, and never uploaded.",
  },
  {
    icon: ArrowLeftRight,
    title: "Ark + Lightning, instant",
    sub: "Spend off-chain VTXO and settle over Lightning in seconds.",
  },
  {
    icon: Wallet,
    title: "On-chain when you need it",
    sub: "Board funds in and exit to the chain whenever you choose.",
  },
];

// BrandPanel is the left trust column shown beside onboarding forms on desktop.
export function BrandPanel({ network }: { network: string }) {
  return (
    <div
      className="relative hidden flex-col justify-between border-r border-border
        [background:var(--surface-alt)] p-12 lg:flex"
    >
      <div className="flex items-center gap-2.5">
        <span className="flex h-9 w-9 items-center justify-center bg-accent">
          <Bitcoin size={18} className="text-white" />
        </span>
        <div>
          <div className="text-sm font-semibold text-fg">WalletDK</div>
          <div className="font-mono text-[11px] tabular-nums text-faint">
            {network} · self-custody
          </div>
        </div>
      </div>

      <div className="max-w-md">
        <h1
          className="text-3xl font-semibold leading-tight tracking-tight text-fg"
        >
          Self-custody that feels effortless.
        </h1>
        <div className="mt-10 space-y-6">
          {BULLETS.map((b) => (
            <div key={b.title} className="flex items-start gap-4">
              <span
                className="flex h-10 w-10 shrink-0 items-center justify-center
                  bg-accent-soft text-accent"
              >
                <b.icon size={18} />
              </span>
              <div>
                <div className="text-sm font-semibold text-fg">{b.title}</div>
                <div className="text-[13px] text-muted">{b.sub}</div>
              </div>
            </div>
          ))}
        </div>
      </div>

      <div className="flex items-center gap-2 text-xs text-faint">
        <Lock size={13} /> Your keys, your coins · open source
      </div>
    </div>
  );
}
