import {
  CircleCheck,
  GitPullRequest,
  Hammer,
  ListChecks,
  type LucideIcon,
  PencilLine,
} from "lucide-react";

import type { BoardColumn } from "@/domain/boardColumns";

/**
 * What tells one column from the next at a glance: an icon for its heading
 * and the tint of the heading's band. The columns are the pipeline's stages,
 * so the factory's three are teal, PR review (on its reviewers) grey and
 * Finished green. Who has to act is not a column's mark: a card that waits
 * on the operator carries its own amber edge. These are a column's marks, not
 * a state's: a state's icon comes from `ui/statusIcons`.
 */
export interface ColumnMark {
  readonly icon: LucideIcon;
  /** The icon's colour. */
  readonly text: string;
  /** The heading band: its tint and its edge. */
  readonly band: string;
  /** What the column holds, in one line: the heading's hover text. */
  readonly meaning: string;
}

const factoryTone = {
  text: "text-tone-info",
  band: "bg-tone-info-soft/60 border-tone-info-border",
} as const;

export const columnMarks: Readonly<Record<BoardColumn, ColumnMark>> = {
  spec: {
    icon: PencilLine,
    ...factoryTone,
    meaning: "The spec and its acceptance tests: drafted by the factory, reviewed by you",
  },
  plan: {
    icon: ListChecks,
    ...factoryTone,
    meaning: "The tickets: planned by the factory, reviewed by you",
  },
  building: {
    icon: Hammer,
    ...factoryTone,
    meaning: "The factory is building tickets in the sandbox",
  },
  prReview: {
    icon: GitPullRequest,
    text: "text-tone-neutral",
    band: "bg-tone-neutral-soft/60 border-tone-neutral-border",
    meaning: "Pull requests are open: the factory answers reviewers, a person merges",
  },
  finished: {
    icon: CircleCheck,
    text: "text-tone-success",
    band: "bg-tone-success-soft/60 border-tone-success-border",
    meaning: "Merged, closed without merging, or cancelled",
  },
};
