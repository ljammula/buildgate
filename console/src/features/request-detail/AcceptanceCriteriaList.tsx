/**
 * The numbered acceptance criteria supplied by the Go request parser, with a
 * count: lets an operator check plan coverage against the factory's
 * numbering without re-parsing the markdown in the browser. Renders nothing
 * when the server has none.
 */
export function AcceptanceCriteriaList({ criteria }: { readonly criteria: readonly string[] }) {
  if (criteria.length === 0) return null;
  return (
    <div data-testid="acceptance-criteria-list" className="flex flex-col gap-1 text-sm">
      <h3 className="font-semibold">{`Acceptance criteria (${criteria.length})`}</h3>
      <ol className="flex flex-col gap-0.5">
        {criteria.map((criterion, i) => (
          <li key={i}>{criterion}</li>
        ))}
      </ol>
    </div>
  );
}
