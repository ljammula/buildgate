import { CloudOff, Loader2 } from "lucide-react";

import { cn } from "@/ui/cn";
import { useNow } from "@/ui/Time";

import { type BoardFreshness, freshnessLabel } from "./boardModel";

export interface FreshnessIndicatorProps {
  readonly freshness: BoardFreshness;
  /** Epoch ms of the last data applied from the poll or the stream; 0 before the first load. */
  readonly lastUpdateAt: number;
}

/** "Live" / "Last updated Ns ago" / "Disconnected", ticking each second. */
export function FreshnessIndicator({ freshness, lastUpdateAt }: FreshnessIndicatorProps) {
  const now = useNow(1000);
  return (
    <span
      data-testid="board-freshness"
      className="text-fg-muted inline-flex items-center gap-1.5 text-xs whitespace-nowrap"
    >
      {freshness === "live" ? (
        <span aria-hidden className="bg-tone-success size-2 rounded-full" />
      ) : freshness === "disconnected" ? (
        <CloudOff aria-hidden className="size-3.5" />
      ) : (
        <Loader2 aria-hidden className={cn("size-3.5")} />
      )}
      {freshnessLabel(freshness, lastUpdateAt === 0 ? null : lastUpdateAt, now)}
    </span>
  );
}
