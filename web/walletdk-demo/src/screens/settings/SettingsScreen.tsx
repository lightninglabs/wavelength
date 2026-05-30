import { ReactNode, useState } from "react";
import {
  ChevronDown,
  Fingerprint,
  Layers,
  type LucideIcon,
  Monitor,
  Power,
  Server,
  Settings as SettingsIcon,
  ShieldCheck,
  Wallet,
  Zap,
} from "lucide-react";
import { WalletInfo } from "@lightninglabs/walletdk-core";
import { GatewayFields } from "../../components/GatewayFields";
import { PageHead } from "../../components/layout/PageHead";
import { AppTab } from "../../components/layout/nav";
import { Band } from "../../components/ui/Band";
import { CopyButton } from "../../components/ui/CopyButton";
import { Label } from "../../components/ui/Label";
import { Segmented } from "../../components/ui/Segmented";
import { SummaryRow } from "../../components/ui/SummaryRow";
import { Toggle } from "../../components/ui/Toggle";
import { cn } from "../../lib/cn";
import { formatSats, shortKey } from "../../lib/format";
import { RuntimeFieldSetter, RuntimeForm } from "../../lib/runtime-config";
import { useTheme } from "../../theme/ThemeProvider";

// TwoCol pairs two compact sections within one band, split by a hairline column
// rule, so the band fills its width instead of stranding a control on the side.
function TwoCol({ left, right }: { left: ReactNode; right: ReactNode }) {
  return (
    <div className="grid gap-y-8 sm:grid-cols-2 sm:gap-y-0">
      <div className="sm:pr-10">{left}</div>
      <div className="sm:border-l sm:border-border sm:pl-10">{right}</div>
    </div>
  );
}

// SettingsScreen surfaces identity, appearance, runtime status, passkey
// security, advanced gateway configuration, build version and the runtime stop
// control, consolidated into full-bleed Zones bands.
export function SettingsScreen({
  info,
  phaseLabel,
  form,
  onField,
  passkeySupported,
  passkeyEnrolled,
  onRemovePasskey,
  onStop,
  onNavigate,
}: {
  info: WalletInfo | null;
  phaseLabel: string;
  form: RuntimeForm;
  onField: RuntimeFieldSetter;
  passkeySupported: boolean;
  passkeyEnrolled: boolean;
  onRemovePasskey: () => void;
  onStop: () => void;
  onNavigate: (tab: AppTab) => void;
}) {
  const { theme, setTheme } = useTheme();
  const [advanced, setAdvanced] = useState(false);
  const identity = info?.IdentityPubKey || "";

  const runtime: Array<{
    icon: LucideIcon;
    label: string;
    value: string;
    good?: boolean;
  }> = [
    { icon: ShieldCheck, label: "Phase", value: phaseLabel, good: true },
    { icon: Zap, label: "Network", value: info?.Network || "—" },
    { icon: Wallet, label: "Wallet", value: info?.WalletType || "—" },
    {
      icon: Server,
      label: "Server",
      value: info?.ServerConnected ? "Connected" : "Offline",
      good: info?.ServerConnected,
    },
    {
      icon: Layers,
      label: "Block height",
      value: info?.BlockHeight ? formatSats(info.BlockHeight) : "—",
    },
  ];

  return (
    <div>
      <PageHead
        title="Settings"
        subtitle="Identity, appearance, security and runtime"
        onBack={() => onNavigate("home")}
      />

      <Band>
        <Label>Runtime</Label>
        <div className="mt-4 flex flex-wrap divide-border sm:divide-x">
          {runtime.map((r) => (
            <div key={r.label} className="flex-1 px-0 sm:px-5 sm:first:pl-0">
              <div className="flex items-center gap-1.5 text-xs text-muted">
                <r.icon
                  size={13}
                  className={r.good ? "text-good" : "text-muted"}
                />
                {r.label}
              </div>
              <div
                className={cn(
                  "mt-1 font-mono text-sm tabular-nums",
                  r.good ? "text-good" : "text-fg",
                )}
              >
                {r.value}
              </div>
            </div>
          ))}
        </div>
      </Band>

      <Band tinted>
        <TwoCol
          left={
            <>
              <Label>Identity</Label>
              <div className="mt-3 flex items-center justify-between gap-3">
                <span className="break-all font-mono text-sm text-fg">
                  {identity ? shortKey(identity, 10, 8) : "—"}
                </span>
                {identity ? <CopyButton value={identity} /> : null}
              </div>
            </>
          }
          right={
            <>
              <Label>About</Label>
              <div className="mt-3 space-y-2.5 text-sm">
                <SummaryRow label="Version" value={info?.Version || "—"} mono />
                <SummaryRow label="Commit" value={info?.Commit || "—"} mono />
              </div>
            </>
          }
        />
      </Band>

      <Band>
        <TwoCol
          left={
            <>
              <Label>Security</Label>
              <div className="mt-3 flex items-center justify-between gap-4">
                <div className="flex items-center gap-2.5">
                  <Fingerprint size={16} className="text-accent" />
                  <div>
                    <div className="text-sm font-medium text-fg">
                      Passkey unlock
                    </div>
                    <div className="text-xs text-muted">
                      {!passkeySupported
                        ? "Not supported on this device"
                        : passkeyEnrolled
                          ? "Enabled · toggle off to remove"
                          : "Set up when you create or unlock"}
                    </div>
                  </div>
                </div>
                <Toggle
                  on={passkeySupported && passkeyEnrolled}
                  onChange={(next) => {
                    if (!next && passkeyEnrolled) {
                      onRemovePasskey();
                    }
                  }}
                  ariaLabel="Passkey unlock"
                />
              </div>
            </>
          }
          right={
            <>
              <Label>Appearance</Label>
              <div className="mt-3 flex items-center justify-between gap-4">
                <div className="flex items-center gap-2.5">
                  <Monitor size={16} className="text-muted" />
                  <div className="text-sm font-medium text-fg">Theme</div>
                </div>
                <Segmented
                  size="sm"
                  value={theme}
                  onChange={(t) => setTheme(t)}
                  options={[
                    { value: "light", label: "Light" },
                    { value: "dark", label: "Dark" },
                  ]}
                />
              </div>
            </>
          }
        />
      </Band>

      <Band tinted>
        <TwoCol
          left={
            <>
              <Label>Advanced</Label>
              <button
                type="button"
                onClick={() => setAdvanced((v) => !v)}
                className="mt-3 flex w-full items-center justify-between"
              >
                <span className="flex items-center gap-2">
                  <SettingsIcon size={15} className="text-muted" />
                  <span className="text-sm font-medium text-fg">
                    Network gateways
                  </span>
                </span>
                <ChevronDown
                  size={16}
                  className={cn(
                    "text-muted transition-transform",
                    advanced && "rotate-180",
                  )}
                />
              </button>
            </>
          }
          right={
            <>
              <Label>Danger zone</Label>
              <div className="mt-3">
                <button
                  type="button"
                  onClick={onStop}
                  className="inline-flex items-center justify-center gap-2 border
                    border-bad bg-bad/10 px-4 py-2.5 text-sm font-semibold
                    text-bad transition-opacity hover:opacity-90"
                >
                  <Power size={16} /> Stop runtime
                </button>
              </div>
            </>
          }
        />
        {advanced ? (
          <div className="mt-6 border-t border-border pt-6">
            <p className="mb-4 text-xs text-muted">
              Display only — the running configuration cannot be changed. Stop
              the runtime to reconnect with different gateways.
            </p>
            <GatewayFields form={form} onField={onField} disabled />
          </div>
        ) : null}
      </Band>
    </div>
  );
}
