import { parseAcceptanceCriteria } from "./requestDetailLogic";

/**
 * The numbered acceptance criteria parsed from the current spec text, with a
 * count: lets an operator check plan and oracle coverage against the spec's
 * own numbering without hunting through the markdown. Renders nothing when
 * the spec has none.
 */
export function AcceptanceCriteriaList({ spec }: { readonly spec: string }) {
  const criteria = parseAcceptanceCriteria(spec);
  if (criteria.length === 0) return null;
  return (
    <div data-testid="acceptance-criteria-list" className="flex flex-col gap-1 text-sm">
      <h3 className="font-semibold">{`Acceptance criteria (${criteria.length})`}</h3>
      <ol className="flex flex-col gap-0.5">
        {criteria.map((criterion, i) => (
          <li key={i}>{`${i + 1}. ${criterion}`}</li>
        ))}
      </ol>
    </div>
  );
}
