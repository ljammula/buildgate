import {
  CircleAlert,
  CircleCheck,
  CircleCheckBig,
  CircleHelp,
  CircleX,
  RefreshCw,
  TriangleAlert,
} from "lucide-react";

import {
  type Brightness,
  type Status,
  type StatusIcon,
  type StatusTone,
  killSwitchDisplay,
  stateLabel,
  statusForToken,
  statusIcon,
  statusTone,
} from "@/domain/status";
import { cn } from "@/ui/cn";
import { toneClasses } from "@/ui/tone";

/** The glyph for a domain StatusIcon name. Decorative: the chip's text carries the meaning. */
export function StatusIconGlyph({ icon, className }: { icon: StatusIcon; className?: string }) {
  const props = { "aria-hidden": true, className: cn("size-3.5 shrink-0", className) };
  switch (icon) {
    case "priority_high":
      return <CircleAlert {...props} />;
    case "autorenew":
      return <RefreshCw {...props} />;
    case "check_circle":
      return <CircleCheck {...props} />;
    case "error":
      return <CircleX {...props} />;
    case "help_outline":
      return <CircleHelp {...props} />;
    case "warning_amber":
      return <TriangleAlert {...props} />;
    case "check_circle_outline":
      return <CircleCheckBig {...props} />;
    default: {
      const unreachable: never = icon;
      return unreachable;
    }
  }
}

interface ChipFrameProps {
  readonly tone: StatusTone;
  readonly outlined: boolean;
  readonly icon: StatusIcon;
  readonly text: string;
  readonly title?: string | undefined;
}

function ChipFrame({ tone, outlined, icon, text, title }: ChipFrameProps) {
  const classes = toneClasses[tone];
  return (
    <span
      data-tone={tone}
      title={title}
      className={cn(
        "inline-flex items-center gap-1 rounded-full border px-2 py-0.5 text-xs font-medium whitespace-nowrap",
        classes.text,
        classes.border,
        !outlined && classes.soft,
      )}
    >
      <StatusIconGlyph icon={icon} />
      {text}
    </span>
  );
}

export interface StatusChipProps {
  readonly status: Status;
  /**
   * The raw state token (or an already-composed label); shown through
   * stateLabel, with the raw token as a tooltip when they differ.
   */
  readonly label: string;
  readonly brightness?: Brightness;
}

/** A pill with the status icon and the operator word for `label`. */
export function StatusChip({ status, label, brightness = "dark" }: StatusChipProps) {
  const shown = stateLabel(label);
  return (
    <ChipFrame
      tone={statusTone(status, brightness)}
      outlined={status === "unknown"}
      icon={statusIcon(status)}
      text={shown}
      title={shown === label ? undefined : label}
    />
  );
}

/** StatusChip for a raw state token, mapping it to its Status first. */
export function StatusChipForToken({ token }: { token: string }) {
  return <StatusChip status={statusForToken(token)} label={token} />;
}

export interface KillSwitchChipProps {
  /** null exactly when the switch's state could not be determined: shown as unknown, never as clear. */
  readonly engaged: boolean | null;
  readonly brightness?: Brightness;
}

/** The kill switch's own three-state chip: engaged, clear, or unknown. */
export function KillSwitchChip({ engaged, brightness = "dark" }: KillSwitchChipProps) {
  const display = killSwitchDisplay(engaged, brightness);
  return (
    <ChipFrame
      tone={display.tone}
      outlined={display.outlined}
      icon={display.icon}
      text={display.label}
    />
  );
}
