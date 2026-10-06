import { computeTicketRollup } from "@/domain/ticketRollup";
import type { RequestSummary } from "@/domain/request";
import { statusTone } from "@/domain/status";
import { toneClasses } from "@/ui/tone";
import { cn } from "@/ui/cn";

/**
 * The compact "3 done · 1 changes requested · 2 building" strip, one
 * coloured dot per bucket. Renders nothing for a request with no tickets
 * yet, so a caller can include it unconditionally.
 */
export function TicketRollupStrip({ request }: { request: RequestSummary }) {
  const entries = computeTicketRollup(request.tickets, request);
  if (entries.length === 0) return null;
  return (
    <ul className="text-fg-muted flex flex-wrap items-center gap-x-3 gap-y-1 text-xs">
      {entries.map((entry) => {
        const tone = statusTone(entry.status, "dark");
        return (
          <li key={entry.label} className="flex items-center gap-1">
            <span
              aria-hidden
              data-tone={tone}
              className={cn("size-2.5 rounded-full bg-current", toneClasses[tone].text)}
            />
            {`${entry.count} ${entry.label}`}
          </li>
        );
      })}
    </ul>
  );
}
