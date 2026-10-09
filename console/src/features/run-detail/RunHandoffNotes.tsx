import { type HandoffNotes, handoffNoteSections } from "@/domain/handoff";
import { EscapedText } from "@/shared/oracle/EscapedText";

export interface RunHandoffNotesProps {
  readonly notes: HandoffNotes | null;
}

/**
 * The build agent's own notes for whoever attempts the ticket next, last in
 * the handoff card and apart from the factory's record. Nothing when it left
 * none.
 *
 * The agent wrote every item, so each is rendered as text only: no Markdown,
 * no link, hidden characters written out. The notes come from the handoff
 * route and are shown nowhere else.
 */
export function RunHandoffNotes({ notes }: RunHandoffNotesProps) {
  const sections = handoffNoteSections(notes);
  if (sections.length === 0) return null;
  return (
    <div
      data-testid="run-handoff-notes"
      className="flex flex-col gap-2 border-t border-border pt-3"
    >
      <h3 className="text-sm font-semibold text-fg">
        The build agent&apos;s own notes (unverified)
      </h3>
      <p className="text-xs text-fg-muted">
        The agent that made this attempt wrote these. They are its view, not the factory&apos;s
        record: where they disagree with anything above, the record above is right.
      </p>
      {sections.map((section) => (
        <div key={section.label} className="flex flex-col gap-0.5">
          <h4 className="text-xs font-medium text-fg">{section.label}</h4>
          <ul className="flex list-disc flex-col gap-0.5 pl-5 text-xs text-fg-muted">
            {section.items.map((item, i) => (
              // Items are positional and never reordered.
              <li key={i}>
                <EscapedText text={item} />
              </li>
            ))}
          </ul>
        </div>
      ))}
    </div>
  );
}
