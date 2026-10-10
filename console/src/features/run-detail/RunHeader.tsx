import { ArrowRight } from "lucide-react";
import { Link } from "react-router";

import { useRequest } from "@/api/requestQueries";
import { requestShortTitle } from "@/domain/request";
import type { Run } from "@/domain/run";
import { requestPath, runsPath } from "@/routes/paths";
import { Button } from "@/ui/Button";
import { CompactId } from "@/ui/CompactId";
import { PageHeader } from "@/ui/PageLayout";
import { StatusChipForToken } from "@/ui/StatusChip";
import { StallChip } from "@/ui/Time";

interface RunHeaderViewProps {
  readonly run: Run;
  readonly title: string;
}

function RunHeaderView({ run, title }: RunHeaderViewProps) {
  return (
    <PageHeader
      title={title}
      breadcrumbs={<Link to={runsPath()}>Back to Runs</Link>}
      description={
        <span className="inline-flex flex-wrap items-center gap-2">
          <CompactId value={run.ticket} max={34} label="ticket id" className="text-xs" />
          <CompactId value={run.id} max={34} label="run id" className="text-xs" />
          <StatusChipForToken token={run.state} />
          <StallChip run={run} />
        </span>
      }
      actions={
        run.requestId === "" ? null : (
          <Button asChild>
            <Link to={requestPath(run.requestId)}>
              Open request
              <ArrowRight aria-hidden="true" />
            </Link>
          </Button>
        )
      }
    />
  );
}

// The run's own request supplies the title. A nice-to-have receipt: nothing
// depends on it, so until it loads (or when the fetch fails) the ticket id
// is the title. It is the query the request page uses, so no second request
// is made for a request already on screen.
function LinkedRunHeader({ run }: { readonly run: Run }) {
  const request = useRequest(run.requestId);
  return (
    <RunHeaderView
      run={run}
      title={request.data === undefined ? run.ticket : requestShortTitle(request.data)}
    />
  );
}

/**
 * The run page's header, in the shape of the request page's: a way back, the
 * request's short title (the ticket id for a run with no request, or until
 * the request loads), then one row of ticket id, run id, state and stall
 * chips, and on the right the way to the request.
 */
export function RunHeader({ run }: { readonly run: Run }) {
  return run.requestId === "" ? (
    <RunHeaderView run={run} title={run.ticket} />
  ) : (
    <LinkedRunHeader run={run} />
  );
}
