// The plan_review panel: each ticket's materialized `<NNN>.oracle/` files,
// which plan approval hash-pins. Renders nothing when no ticket has any;
// reports what it has shown through onChanged so the screen's Approve can
// wait for it and send those hashes with the spec hashes.
import { RefreshCw } from "lucide-react";
import { useEffect, useRef } from "react";
import { useQueries } from "@tanstack/react-query";

import { useApi } from "@/api/ApiProvider";
import { getRequestTicketOracle, getRequestTicketOracleFile } from "@/api/oracle";
import { queryKeys } from "@/api/queryKeys";
import { ApiError } from "@/domain/apiError";
import type { OracleListing } from "@/domain/oracle";
import type { RequestSummary } from "@/domain/request";
import { OracleFileTile } from "@/shared/oracle/OracleFileTile";
import { OracleProblems } from "@/shared/oracle/OracleProblems";
import { StaleListingNotice } from "@/shared/oracle/StaleListingNotice";
import { ticketOraclePrefix } from "@/shared/oracle/ticketOraclePrefix";
import { type OracleFileSource, useOracleFileStore } from "@/shared/oracle/useOracleFileStore";
import { Button } from "@/ui/Button";

/**
 * What a TicketOraclePanel has displayed: every shown file's approval key
 * (`tickets/NNN.oracle/name`) -> sha256 of the bytes received, and whether
 * approval may proceed (all listings loaded fresh, no problems, every file
 * shown).
 */
export interface TicketOracleShown {
  readonly hashes: Readonly<Record<string, string>>;
  readonly complete: boolean;
}

export interface TicketOraclePanelProps {
  /** The request at plan_review; its tickets that have a spec path are listed. */
  readonly request: RequestSummary;
  /**
   * Called on mount and whenever what is shown changes (a tile opened or
   * closed, a reload, a changed file). Until `complete` is true the plan's
   * Approve must stay disabled; once true, merge `hashes` into the
   * `expected_sha256` sent with the spec hashes. A remount starts over:
   * nothing counts as shown until re-opened.
   */
  readonly onChanged: (shown: TicketOracleShown) => void;
}

// A server without the route (or a ticket it does not know) has no
// materialized files to show.
const noFiles: OracleListing = {
  files: [],
  problems: [],
  state: "",
  draftStatus: "",
  draftDetail: "",
  proposedCommand: "",
};

/**
 * The plan_review panel: each ticket's `<NNN>.oracle/` files, read-only,
 * with the shown-hash map reported through `onChanged`. Renders nothing while
 * no ticket has files or problems and nothing failed.
 */
export function TicketOraclePanel({ request, onChanged }: TicketOraclePanelProps) {
  const { http } = useApi();
  const id = request.id;
  const groups = request.tickets
    .filter((t) => t.specPath !== "")
    .map((t) => ({ index: t.index, prefix: ticketOraclePrefix(t.specPath) }));

  const listingQueries = useQueries({
    queries: groups.map((g) => ({
      queryKey: queryKeys.requests.ticketOracle(id, g.index),
      queryFn: async ({ signal }: { signal: AbortSignal }) => {
        try {
          return await getRequestTicketOracle(http, id, g.index, signal);
        } catch (error) {
          if (error instanceof ApiError && error.status === 404) return noFiles;
          throw error;
        }
      },
    })),
  });

  const loaded = groups.map((g, i) => ({ ...g, listing: listingQueries[i]?.data }));
  const failure = listingQueries.find((q) => q.isError)?.error;
  const loading = listingQueries.some((q) => q.isFetching);

  const sources: OracleFileSource[] = loaded.flatMap((g) =>
    (g.listing?.files ?? []).map((f) => ({
      key: `${g.prefix}/${f.name}`,
      sha256: f.sha256,
      queryKey: queryKeys.requests.ticketOracleFile(id, g.index, f.name),
      fetch: (signal: AbortSignal) => getRequestTicketOracleFile(http, id, g.index, f.name, signal),
    })),
  );
  const store = useOracleFileStore(sources);
  const shown = Object.fromEntries(store.shown);
  const complete =
    !loading &&
    failure === undefined &&
    loaded.every((g) => g.listing !== undefined && g.listing.problems.length === 0) &&
    store.shown.size === sources.length;

  // Tell the parent what is displayed, once per change. The signature is the
  // whole payload, so an unchanged render reports nothing.
  const signature = JSON.stringify([complete, Object.entries(shown).sort()]);
  const reported = useRef<string | null>(null);
  useEffect(() => {
    if (reported.current === signature) return;
    reported.current = signature;
    onChanged({ hashes: shown, complete });
  }, [signature, shown, complete, onChanged]);

  const withFiles = loaded.filter((g) => (g.listing?.files.length ?? 0) > 0);
  const withProblems = loaded.filter((g) => (g.listing?.problems.length ?? 0) > 0);
  if (withFiles.length === 0 && withProblems.length === 0 && failure === undefined) {
    return <div data-testid="ticket-oracle-panel" />;
  }

  const reload = async () => {
    await Promise.all(listingQueries.map((q) => q.refetch()));
  };

  return (
    <div data-testid="ticket-oracle-panel" className="flex flex-col items-stretch gap-2">
      <h3 className="text-base font-semibold">Ticket oracle files</h3>
      <p className="text-sm">
        Approving the plan pins these acceptance tests by hash. Open every file to enable Approve.
      </p>
      {failure !== undefined ? <StaleListingNotice error={failure} /> : null}
      {withProblems.map((g) => (
        <OracleProblems key={g.index} problems={g.listing?.problems ?? []} />
      ))}
      {withFiles.map((g) => (
        <div key={g.index}>
          <h4 className="pt-2 text-sm font-semibold">{`Ticket ${g.index} (${g.prefix}/)`}</h4>
          {(g.listing?.files ?? []).map((file) => {
            const storeKey = `${g.prefix}/${file.name}`;
            return (
              <OracleFileTile
                key={storeKey}
                keyId={`${g.index}/${file.name}`}
                storeKey={storeKey}
                file={file}
                store={store}
                shown={store.shown.has(storeKey)}
              />
            );
          })}
        </div>
      ))}
      <div className="flex items-center gap-2">
        <Button disabled={loading} onClick={() => void reload()}>
          <RefreshCw aria-hidden="true" />
          Reload oracle files
        </Button>
        <span data-testid="ticket-oracle-count" className="text-sm">
          {`${store.shown.size} of ${sources.length} shown`}
        </span>
      </div>
    </div>
  );
}
