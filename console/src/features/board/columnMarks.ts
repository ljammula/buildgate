import {
  CircleAlert,
  CircleCheck,
  GitPullRequest,
  Hammer,
  type LucideIcon,
  PencilLine,
} from "lucide-react";

import type { BoardColumn } from "@/domain/boardColumns";

/**
 * What tells one column from the next at a glance: an icon for its heading
 * and the tint of the heading's band. The tone is who has the work: amber
 * waits on the operator, teal is the factory's, grey waits on a reviewer,
 * green is finished. These are a column's marks, not a state's: a state's
 * icon comes from `ui/statusIcons`.
 */
export interface ColumnMark {
  readonly icon: LucideIcon;
  /** The icon's colour. */
  readonly text: string;
  /** The heading band: its tint and its edge. */
  readonly band: string;
  /** Whose move it is, in one line: the heading's hover text. */
  readonly meaning: string;
}

export const columnMarks: Readonly<Record<BoardColumn, ColumnMark>> = {
  drafting: {
    icon: PencilLine,
    text: "text-tone-info",
    band: "bg-tone-info-soft/60 border-tone-info-border",
    meaning: "The factory is writing the spec, the oracles or the plan",
  },
  needsYou: {
    icon: CircleAlert,
    text: "text-tone-warning",
    band: "bg-tone-warning-soft/60 border-tone-warning-border",
    meaning: "Waiting on you: a review to pass, or something stuck to unblock",
  },
  building: {
    icon: Hammer,
    text: "text-tone-info",
    band: "bg-tone-info-soft/60 border-tone-info-border",
    meaning: "The factory is building tickets in the sandbox",
  },
  prReview: {
    icon: GitPullRequest,
    text: "text-tone-neutral",
    band: "bg-tone-neutral-soft/60 border-tone-neutral-border",
    meaning: "Pull requests are open and wait on their reviewers",
  },
  done: {
    icon: CircleCheck,
    text: "text-tone-success",
    band: "bg-tone-success-soft/60 border-tone-success-border",
    meaning: "Finished requests",
  },
};
