import { safeHttpUrl } from "@/domain/safeUrl";
import { ArrowRight, ExternalLink } from "lucide-react";

import { describeError } from "@/ui/ErrorDisplay";
import { formatModelUsageLine } from "@/domain/cost";
import type { Run } from "@/domain/run";
import { baselineVerifyFailed, baselineVerifySummary, runTokensText } from "@/domain/runSummary";
import { cn } from "@/ui/cn";
import { Badge } from "@/ui/Badge";
import { Button } from "@/ui/Button";
import { CompactId } from "@/ui/CompactId";
import { CopyButton } from "@/ui/CopyButton";
import { Disclosure } from "@/ui/Disclosure";
import { DescriptionItem, DescriptionList } from "@/ui/DescriptionList";
import { Section } from "@/ui/PageLayout";
import { RelativeTime } from "@/ui/RelativeTime";
import { StatusChipForToken } from "@/ui/StatusChip";

// Past this many files the list is folded; the diff view has them all.
const FILES_SHOWN = 8;

export interface RunFactsCardProps {
  readonly run: Run;
  readonly streamError: unknown;
  readonly temporalUiUrl: string | null;
}

function ChangedFiles({ run }: { readonly run: Run }) {
  const files = run.changedFiles ?? [];
  const diff = run.diffStat;
  if (files.length === 0 && diff === null) return null;
  const rest = files.slice(FILES_SHOWN);
  return (
    <div className="flex flex-col gap-1.5 border-t border-border pt-3">
      <h3 className="text-xs font-semibold text-fg-muted">Changed files</h3>
      {diff === null ? null : (
        <p className="text-sm tabular-nums">
          {`${diff.filesChanged} ${diff.filesChanged === 1 ? "file" : "files"} +${diff.insertions} −${diff.deletions}`}
        </p>
      )}
      <ul className="flex flex-col gap-0.5 font-mono text-xs break-all">
        {files.slice(0, FILES_SHOWN).map((file) => (
          <li key={file}>{file}</li>
        ))}
      </ul>
      {rest.length === 0 ? null : (
        <Disclosure bare title={`${rest.length} more`} headingLevel="h3">
          <ul className="flex flex-col gap-0.5 font-mono text-xs break-all">
            {rest.map((file) => (
              <li key={file}>{file}</li>
            ))}
          </ul>
        </Disclosure>
      )}
    </div>
  );
}

/**
 * The run's one facts card: state, id, age and usage; what it ran against and
 * produced (base and result SHAs, the spec's hash, whether factoryd made the
 * commit); and the files it changed. Facts the run does not have yet are
 * left out, not printed as "Not available".
 */
export function RunFactsCard({ run, streamError, temporalUiUrl }: RunFactsCardProps) {
  // Offered only when the server advertises a Temporal UI and this run
  // recorded its workflow id: either missing means there is nothing to link to.
  // The address comes from the server's configuration: only an http(s) one
  // is made a link.
  const temporalBase = temporalUiUrl === null ? null : safeHttpUrl(temporalUiUrl);
  const temporalHref =
    temporalBase !== null && run.temporalWorkflowId !== ""
      ? `${temporalBase.replace(/\/$/, "")}/namespaces/default/workflows/${encodeURIComponent(run.temporalWorkflowId)}`
      : null;
  // Tokens only: the operator explicitly does not want a dollar figure here.
  const tokens = runTokensText(run);
  return (
    <Section title="Run" card>
      <DescriptionList labelWidth="sm">
        <DescriptionItem label="State">
          <StatusChipForToken token={run.state} />
        </DescriptionItem>
        <DescriptionItem label="Run ID" mono>
          <CompactId value={run.id} max={22} label="run id" />
        </DescriptionItem>
        <DescriptionItem label="Updated">
          <RelativeTime value={run.updatedAt} />
        </DescriptionItem>
        {tokens === null ? null : (
          <DescriptionItem label="Usage">
            <span>{tokens}</span>
            <Disclosure bare title="By model" headingLevel="h3">
              <ul className="text-fg-muted text-xs">
                {run.byModel.map((m) => (
                  <li key={`${m.role}-${m.model}`}>
                    {formatModelUsageLine(m, run.tokensComplete)}
                  </li>
                ))}
              </ul>
            </Disclosure>
          </DescriptionItem>
        )}
        {run.baselineVerify === null ? null : (
          <DescriptionItem label="Baseline verify">
            <span
              data-testid="baseline-verify"
              data-failed={baselineVerifyFailed(run.baselineVerify)}
              className={cn(
                baselineVerifyFailed(run.baselineVerify) && "font-medium text-tone-danger",
              )}
            >
              {baselineVerifySummary(run.baselineVerify)}
            </span>
          </DescriptionItem>
        )}
        {streamError !== null ? (
          <DescriptionItem label="Live updates">
            Disconnected: {describeError(streamError).raw}
          </DescriptionItem>
        ) : null}
      </DescriptionList>
      {run.baseSha === "" && run.resultSha === null ? null : (
        <div className="flex flex-col gap-1.5 border-t border-border pt-3">
          <h3 className="text-xs font-semibold text-fg-muted">Commit and artifact evidence</h3>
          <div className="flex flex-wrap items-center gap-x-1.5 gap-y-1 text-xs">
            <CompactId value={run.baseSha} max={14} label="base SHA" />
            {run.resultSha === null ? null : (
              <>
                <ArrowRight aria-hidden="true" className="text-fg-subtle size-3" />
                <CompactId value={run.resultSha} max={14} label="result SHA" />
                {run.committedByFactoryd ? <Badge>Committed by factoryd</Badge> : null}
              </>
            )}
          </div>
          <p className="text-fg-muted flex items-center gap-1 text-xs">
            Spec SHA-256
            <CompactId value={run.specSha256} max={14} label="spec SHA-256" className="text-xs" />
          </p>
        </div>
      )}
      <ChangedFiles run={run} />
      {temporalHref !== null ? (
        <div className="flex items-center gap-1">
          <Button asChild>
            <a href={temporalHref} target="_blank" rel="noopener noreferrer">
              <ExternalLink aria-hidden="true" />
              Open in Temporal UI
            </a>
          </Button>
          <CopyButton size="sm" text={temporalHref} label="Copy Temporal link" />
        </div>
      ) : null}
    </Section>
  );
}
