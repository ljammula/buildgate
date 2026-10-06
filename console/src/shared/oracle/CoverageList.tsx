import type { OracleManifestEntry } from "@/domain/oracle";
import { EscapedText } from "@/shared/oracle/EscapedText";

export interface CoverageListProps {
  readonly entries: readonly OracleManifestEntry[];
}

function summary(e: OracleManifestEntry): string {
  const head = `${e.criterionIndex === null ? "-" : `${e.criterionIndex}.`} ${e.criterion}`;
  const covered =
    e.oracleFile === null
      ? `Not covered by an oracle${e.rationale === "" ? "" : `: ${e.rationale}`}`
      : `Covered by ${e.oracleFile}${e.targetPath === "" ? "" : ` (tests ${e.targetPath})`}`;
  const supersedes = e.supersedes.length === 0 ? "" : `\n   Supersedes: ${e.supersedes.join(", ")}`;
  return `${head}\n   ${covered}${supersedes}`;
}

/** Which spec criterion each oracle file covers, from MANIFEST.json. */
export function CoverageList({ entries }: CoverageListProps) {
  return (
    <div data-testid="oracle-coverage" className="flex flex-col gap-1">
      <h3 className="text-sm font-semibold">Criteria coverage</h3>
      {entries.length === 0 ? <p className="text-sm">MANIFEST.json lists no criteria.</p> : null}
      {entries.map((e, index) => (
        <EscapedText key={index} text={summary(e)} className="text-sm" />
      ))}
    </div>
  );
}
