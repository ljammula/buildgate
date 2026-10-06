// The plan_review panel: each ticket's materialized `<NNN>.oracle/` files,
// which plan approval hash-pins. Renders nothing when no ticket has any;
// reports what it has shown through onChanged so the screen's Approve can
// wait for it and send those hashes with the spec hashes.
import { RefreshCw } from "lucide-react";
import { useEffect, useState } from "react";

import { useTicketOracleListings } from "@/api/requestQueries";
import type { RequestSummary } from "@/domain/request";
import { OracleFileTile } from "@/shared/oracle/OracleFileTile";
import { OracleProblems } from "@/shared/oracle/OracleProblems";
import { ticketOraclePrefix } from "@/shared/oracle/ticketOraclePrefix";
import { type OracleFileSource, useOracleFileStore } from "@/shared/oracle/useOracleFileStore";
import { Button } from "@/ui/Button";
import { StaleWarning } from "@/ui/StaleWarning";

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

function sameShown(a: TicketOracleShown, b: TicketOracleShown): boolean {
  if (a.complete !== b.complete) return false;
  const left = Object.entries(a.hashes);
  return (
    left.length === Object.keys(b.hashes).length &&
    left.every(([key, hash]) => b.hashes[key] === hash)
  );
}

/** `value`, but the previous object while nothing in it changed. */
function useStableShown(value: TicketOracleShown): TicketOracleShown {
  const [stable, setStable] = useState(value);
  if (sameShown(stable, value)) return stable;
  setStable(value);
  return value;
}

/**
 * The plan_review panel: each ticket's `<NNN>.oracle/` files, read-only,
 * with the shown-hash map reported through `onChanged`. Renders nothing while
 * no ticket has files or problems and nothing failed.
 */
export function TicketOraclePanel({ request, onChanged }: TicketOraclePanelProps) {
  const id = request.id;
  const groups = request.tickets
    .filter((t) => t.specPath !== "")
    .map((t) => ({ index: t.index, prefix: ticketOraclePrefix(t.specPath) }));

  const listingQueries = useTicketOracleListings(
    id,
    groups.map((g) => g.index),
  );

  const loaded = groups.map((g, i) => ({ ...g, listing: listingQueries[i]?.data }));
  const failure = listingQueries.find((q) => q.isError)?.error;
  const loading = listingQueries.some((q) => q.isFetching);

  const sources: OracleFileSource[] = loaded.flatMap((g) =>
    (g.listing?.files ?? []).map((f) => ({
      key: `${g.prefix}/${f.name}`,
      sha256: f.sha256,
      name: f.name,
      ticket: g.index,
    })),
  );
  const store = useOracleFileStore(id, sources);
  const complete =
    !loading &&
    failure === undefined &&
    loaded.every((g) => g.listing !== undefined && g.listing.problems.length === 0) &&
    store.shown.size === sources.length;

  // What the parent is told: one object that keeps its identity until a hash
  // or `complete` actually changes (`store.shown` is a new Map every render).
  const reported = useStableShown({ hashes: Object.fromEntries(store.shown), complete });
  useEffect(() => {
    onChanged(reported);
  }, [reported, onChanged]);

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
      {failure !== undefined ? (
        <StaleWarning error={failure} detail="callout" testId="oracle-stale-listing">
          The files below are the last listing that loaded and may be out of date -- Approve is
          disabled until Reload files succeeds.
        </StaleWarning>
      ) : null}
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
