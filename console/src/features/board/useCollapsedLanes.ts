import { useState } from "react";

import { getCollapsedLanes, setCollapsedLanes } from "@/platform/boardPrefs";

export interface CollapsedLanes {
  readonly isCollapsed: (project: string) => boolean;
  readonly toggle: (project: string) => void;
}

/** Which project lanes are folded, remembered per browser. */
export function useCollapsedLanes(): CollapsedLanes {
  const [collapsed, setCollapsed] = useState<ReadonlySet<string>>(
    () => new Set(getCollapsedLanes()),
  );
  return {
    isCollapsed: (project) => collapsed.has(project),
    toggle: (project) => {
      const next = new Set(collapsed);
      if (!next.delete(project)) next.add(project);
      setCollapsedLanes([...next]);
      setCollapsed(next);
    },
  };
}
