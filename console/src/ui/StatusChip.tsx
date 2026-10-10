import {
  type Brightness,
  type Status,
  type StatusIcon,
  type StatusTone,
  killSwitchDisplay,
  stateLabel,
  statusForToken,
  statusIcon,
  statusIconForToken,
  statusTone,
} from "@/domain/status";
import { cn } from "@/ui/cn";
import { statusIcons } from "@/ui/statusIcons";
import { toneClasses } from "@/ui/tone";

/** The glyph for a domain StatusIcon name. Decorative: the chip's text carries the meaning. */
function StatusIconGlyph({ icon }: { icon: StatusIcon }) {
  const Icon = statusIcons[icon];
  return <Icon aria-hidden="true" className="size-3.5 shrink-0" />;
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
  /** Overrides the icon of `status`, for a state with a shape of its own (stuck, queued). */
  readonly icon?: StatusIcon;
}

/** A pill with the status icon and the operator word for `label`. */
export function StatusChip({ status, label, brightness = "dark", icon }: StatusChipProps) {
  const shown = stateLabel(label);
  return (
    <ChipFrame
      tone={statusTone(status, brightness)}
      outlined={status === "unknown"}
      icon={icon ?? statusIcon(status)}
      text={shown}
      title={shown === label ? undefined : label}
    />
  );
}

/** StatusChip for a raw state token, mapping it to its Status first. */
export function StatusChipForToken({ token }: { token: string }) {
  return (
    <StatusChip status={statusForToken(token)} label={token} icon={statusIconForToken(token)} />
  );
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
