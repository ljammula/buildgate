import { type RequestSummary, isMemoryRequest } from "@/domain/request";
import { Badge } from "@/ui/Badge";

/**
 * The "memory" label of a request `factoryd memory propose` opened, shown by
 * the board and the request page. Renders nothing for any other request.
 */
export function MemoryBadge({ request }: { readonly request: Pick<RequestSummary, "sourceKind"> }) {
  if (!isMemoryRequest(request)) return null;
  return (
    <Badge
      variant="outline"
      data-testid="request-memory-badge"
      title="A repository memory change: its ticket rewrites the memory section of AGENTS.md"
    >
      memory
    </Badge>
  );
}
