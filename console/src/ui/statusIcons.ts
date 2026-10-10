import {
  CircleAlert,
  CircleCheck,
  CircleCheckBig,
  CircleDashed,
  CircleDot,
  CircleHelp,
  CircleMinus,
  CircleX,
  Hourglass,
  OctagonPause,
  TriangleAlert,
  type LucideIcon,
} from "lucide-react";

import type { StatusIcon } from "@/domain/status";

/**
 * The one map from a state's icon name to its drawing, read by the status
 * chip and the run timeline. A name missing here is a type error.
 */
export const statusIcons: Readonly<Record<StatusIcon, LucideIcon>> = {
  circle_alert: CircleAlert,
  circle_dot: CircleDot,
  circle_check: CircleCheck,
  circle_x: CircleX,
  circle_help: CircleHelp,
  triangle_alert: TriangleAlert,
  circle_check_big: CircleCheckBig,
  octagon_pause: OctagonPause,
  circle_dashed: CircleDashed,
  circle_minus: CircleMinus,
  hourglass: Hourglass,
};
