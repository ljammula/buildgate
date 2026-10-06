import { digestText } from "@/domain/digest";
import { EscapedText } from "@/shared/oracle/EscapedText";
import { Disclosure } from "@/ui/Disclosure";

export interface DigestedTextProps {
  /** Untrusted machine text: a halt reason, an error. */
  readonly text: string;
  /** What the disclosure of the whole text is called. */
  readonly fullLabel?: string;
  readonly max?: number;
  readonly className?: string;
}

/**
 * The first sentence or line of a long machine message, with the whole of it
 * one click away. A message that already fits is shown as it is, with no
 * disclosure. Both parts go through EscapedText: the text is agent-written.
 */
export function DigestedText({
  text,
  fullLabel = "Full text",
  max = 160,
  className,
}: DigestedTextProps) {
  const digest = digestText(text, max);
  return (
    <div className="flex flex-col gap-1">
      <EscapedText text={digest.head} {...(className === undefined ? {} : { className })} />
      {digest.truncated ? (
        <Disclosure bare title={fullLabel} headingLevel="h3">
          <EscapedText text={text} className="text-xs" />
        </Disclosure>
      ) : null}
    </div>
  );
}
