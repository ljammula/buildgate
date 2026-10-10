// The oracle_review panel: files with hashes, raw content per file, MANIFEST
// coverage, approval problems, RUN_COMMAND.txt editing, and an Approve that
// stays disabled until every file has been shown.
//
// Approval pins every file of the directory by hash, and the API
// (request.ApproveShown) refuses an approval that does not name a hash for
// each -- so Approve stays disabled until every listed file is currently
// expanded with content whose hash matches the listing's, and the hashes sent
// are those of the bytes received. A collapsed file counts as not shown, and
// so does everything while the listing is stale (a failed reload).
import { Check, RefreshCw } from "lucide-react";

import { useApi } from "@/api/ApiProvider";
import { useRequestOracle } from "@/api/requestQueries";
import { oracleExpectedSha256 } from "@/domain/contentHash";
import { type OracleFileContent, parseOracleManifest } from "@/domain/oracle";
import type { RequestSummary } from "@/domain/request";
import { CoverageList } from "@/shared/oracle/CoverageList";
import { CriteriaList } from "@/shared/oracle/CriteriaList";
import { EscapedText } from "@/shared/oracle/EscapedText";
import { OracleFileTile } from "@/shared/oracle/OracleFileTile";
import { OracleProblems } from "@/shared/oracle/OracleProblems";
import { RunCommandBlock } from "@/shared/oracle/RunCommandBlock";
import { oracleDraftStatusLabel } from "@/shared/oracle/oracleDraftStatus";
import { type OracleFileSource, useOracleFileStore } from "@/shared/oracle/useOracleFileStore";
import { useRunCommandEditing } from "@/shared/oracle/useRunCommandEditing";
import { Button } from "@/ui/Button";
import { CopyButton } from "@/ui/CopyButton";
import { ErrorCallout } from "@/ui/ErrorDisplay";
import { Spinner } from "@/ui/Feedback";
import { StaleWarning } from "@/ui/StaleWarning";

const RUN_COMMAND_NAME = "RUN_COMMAND.txt";
const MANIFEST_NAME = "MANIFEST.json";

export interface OracleReviewPanelProps {
  /** The request at (or shown as being at) oracle_review; its id picks the oracle and its draft criteria are listed. */
  readonly request: RequestSummary;
  /**
   * False while the parent is mid-action or its own detail load has not
   * finished. True still requires write access (checked here) and every
   * other approval condition.
   */
  readonly canAct: boolean;
  /**
   * Called when Approve is pressed, with the `expected_sha256` map of exactly
   * what was displayed (`oracle/NAME` -> sha256 of the bytes received; `{}`
   * for an empty oracle/, which the parent sends as a skip). The panel
   * re-lists the files once it settles, so a refused approval (files changed
   * underneath the operator) leaves nothing stale; the parent shows the
   * refusal itself.
   */
  readonly onApprove: (expectedSha256: Record<string, string>) => Promise<void>;
}

/**
 * The oracle_review panel (request-level `oracle/`). Owns its Approve
 * button (name "Approve", or "Approve (skip oracle)" for an empty oracle/).
 */
export function OracleReviewPanel({ request, canAct, onApprove }: OracleReviewPanelProps) {
  const { canWrite } = useApi();
  const id = request.id;
  const listingQuery = useRequestOracle(id);
  const runCommand = useRunCommandEditing(id);

  const listing = listingQuery.data;
  const sources: OracleFileSource[] = (listing?.files ?? []).map((f) => ({
    key: f.name,
    sha256: f.sha256,
    name: f.name,
  }));
  const hasManifest = sources.some((s) => s.key === MANIFEST_NAME);
  const store = useOracleFileStore(id, sources, hasManifest ? [MANIFEST_NAME] : []);

  const reload = async () => {
    await listingQuery.refetch();
  };

  if (listing === undefined) {
    return (
      <div data-testid="oracle-review-panel" className="flex flex-col items-start gap-2">
        {listingQuery.isError ? (
          <>
            <ErrorCallout error={listingQuery.error} />
            <Button size="sm" disabled={listingQuery.isFetching} onClick={() => void reload()}>
              Retry
            </Button>
          </>
        ) : (
          <Spinner label="Loading oracle files" />
        )}
      </div>
    );
  }

  const shown = store.shown;
  const unseen = listing.files.length - shown.size;
  const atReview = listing.state === "oracle_review";
  const stale = listingQuery.isError || listingQuery.isFetching;
  const canApprove =
    canAct && canWrite && atReview && listing.problems.length === 0 && unseen === 0 && !stale;
  const manifestText = store.content(MANIFEST_NAME)?.content.text;
  const manifest = manifestText === undefined ? null : parseOracleManifest(manifestText);

  const runCommandBody = (content: OracleFileContent) => (
    <RunCommandBlock
      keyId={RUN_COMMAND_NAME}
      content={content}
      editing={runCommand}
      canWrite={canWrite}
      suggestion={listing.proposedCommand}
      onSaved={() => {
        // The saved file must be re-fetched and re-shown before approval.
        store.reopen(RUN_COMMAND_NAME);
        void reload();
      }}
    />
  );

  return (
    <div data-testid="oracle-review-panel" className="flex flex-col items-stretch gap-3">
      {listingQuery.isError ? (
        <StaleWarning error={listingQuery.error} detail="callout" testId="oracle-stale-listing">
          The files below are the last listing that loaded and may be out of date: Approve is
          disabled until Reload oracle files succeeds.
        </StaleWarning>
      ) : null}
      {listing.draftStatus !== "" ? (
        <div data-testid="oracle-draft-status">
          <EscapedText
            className="text-sm"
            text={`Oracle draft: ${oracleDraftStatusLabel(listing.draftStatus)}${
              listing.draftDetail === "" ? "" : `: ${listing.draftDetail}`
            }`}
          />
        </div>
      ) : null}
      {/* Per-criterion eligibility verdicts: a none_eligible/drafted outcome
          comes with a receipt, not only the free-text status/detail above. */}
      {request.oracleDraftCriteria.length > 0 ? (
        <CriteriaList criteria={request.oracleDraftCriteria} />
      ) : null}
      {listing.proposedCommand !== "" ? (
        <div className="flex flex-col gap-1">
          <p className="text-fg-muted text-xs">
            Suggested RUN_COMMAND.txt (a suggestion only; not written to the file):
          </p>
          <div data-testid="oracle-proposed-command" className="flex items-start gap-1">
            <EscapedText text={listing.proposedCommand} className="font-mono text-xs" />
            <CopyButton
              size="sm"
              text={listing.proposedCommand}
              label="Copy suggested RUN_COMMAND.txt"
            />
          </div>
        </div>
      ) : null}
      {listing.problems.length > 0 ? <OracleProblems problems={listing.problems} /> : null}
      {listing.files.length === 0 ? (
        <p data-testid="oracle-empty" className="text-sm">
          No oracle files. Approving skips the oracle stage; the build then has no request-level
          acceptance test.
        </p>
      ) : (
        <>
          {manifest !== null ? <CoverageList entries={manifest} /> : null}
          <p data-testid="oracle-review-hint" className="text-sm">
            Before approving: check the errors.Is/sentinel assertions (never against a second,
            freshly-constructed error) and whether the oracle reaches beyond its own target
            file/package. A diff_scope quarantine naming a file only touched to satisfy this oracle
            means the oracle may be wrong, not the build.
          </p>
          <div>
            <h3 className="text-sm font-semibold">
              {`Files (${shown.size} of ${listing.files.length} shown): open every one to enable Approve`}
            </h3>
            {listing.files.map((file) => (
              <OracleFileTile
                key={file.name}
                keyId={file.name}
                storeKey={file.name}
                file={file}
                store={store}
                shown={shown.has(file.name)}
                {...(file.name === RUN_COMMAND_NAME && atReview
                  ? { renderBody: runCommandBody }
                  : {})}
              />
            ))}
          </div>
        </>
      )}
      <div className="flex flex-wrap items-center gap-2">
        <Button
          variant="primary"
          disabled={!canApprove}
          onClick={() => {
            // A refused approval (files changed underneath us) leaves stale
            // hashes behind; re-list so the panel matches the server again.
            void (async () => {
              try {
                await onApprove(oracleExpectedSha256(Object.fromEntries(shown)));
              } catch {
                // The parent reports its own refusal; the re-list below is
                // what this panel owes the operator.
              } finally {
                await reload();
              }
            })();
          }}
        >
          <Check aria-hidden="true" />
          {listing.files.length === 0 ? "Approve (skip oracle)" : "Approve"}
        </Button>
        <Button disabled={listingQuery.isFetching} onClick={() => void reload()}>
          <RefreshCw aria-hidden="true" />
          Reload oracle files
        </Button>
        {unseen > 0 ? (
          <span data-testid="oracle-unseen-hint" className="text-sm">
            {`Open ${unseen} more file${unseen === 1 ? "" : "s"} to approve.`}
          </span>
        ) : null}
      </div>
    </div>
  );
}
