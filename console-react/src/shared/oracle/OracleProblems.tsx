import { EscapedText } from "@/shared/oracle/EscapedText";
import { Callout } from "@/ui/Feedback";

export interface OracleProblemsProps {
  /** The problems approval would refuse (untrusted text). */
  readonly problems: readonly string[];
}

/** The approval-blocking problems of an oracle directory. */
export function OracleProblems({ problems }: OracleProblemsProps) {
  return (
    <Callout
      tone="danger"
      title="Approval is blocked until these are fixed:"
      data-testid="oracle-problems"
    >
      {problems.map((problem, index) => (
        <EscapedText key={index} text={`- ${problem}`} />
      ))}
    </Callout>
  );
}
