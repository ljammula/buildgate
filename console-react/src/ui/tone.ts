import type { StatusTone } from "@/domain/status";

/**
 * The classes that draw each domain StatusTone, in one place so a chip, a
 * callout and a table cell colour the same state the same way. The three
 * greys (neutral, neutralOnDark, muted) differ only to stay legible on each
 * brightness; the theme tokens do that.
 */
export interface ToneClasses {
  readonly text: string;
  readonly soft: string;
  readonly border: string;
}

const neutral: ToneClasses = {
  text: "text-tone-neutral",
  soft: "bg-tone-neutral-soft",
  border: "border-tone-neutral-border",
};

export const toneClasses: Readonly<Record<StatusTone, ToneClasses>> = {
  warning: {
    text: "text-tone-warning",
    soft: "bg-tone-warning-soft",
    border: "border-tone-warning-border",
  },
  info: { text: "text-tone-info", soft: "bg-tone-info-soft", border: "border-tone-info-border" },
  success: {
    text: "text-tone-success",
    soft: "bg-tone-success-soft",
    border: "border-tone-success-border",
  },
  danger: {
    text: "text-tone-danger",
    soft: "bg-tone-danger-soft",
    border: "border-tone-danger-border",
  },
  neutral,
  neutralOnDark: neutral,
  muted: neutral,
};
