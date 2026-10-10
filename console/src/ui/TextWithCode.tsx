import { Fragment } from "react";

import { cn } from "@/ui/cn";

export interface TextWithCodeProps {
  readonly text: string;
  readonly className?: string;
}

/** The pieces of `text`: text outside backtick pairs, and the code between them. An unpaired backtick stays as written. */
export function splitBackticks(text: string): { readonly code: boolean; readonly value: string }[] {
  const parts: { code: boolean; value: string }[] = [];
  let rest = text;
  for (;;) {
    const open = rest.indexOf("`");
    const close = open < 0 ? -1 : rest.indexOf("`", open + 1);
    if (close < 0) break;
    if (open > 0) parts.push({ code: false, value: rest.slice(0, open) });
    // An empty pair is not code: it stays as written.
    if (close === open + 1) parts.push({ code: false, value: "``" });
    else parts.push({ code: true, value: rest.slice(open + 1, close) });
    rest = rest.slice(close + 1);
  }
  if (rest !== "") parts.push({ code: false, value: rest });
  return parts;
}

/**
 * Text with the words between backticks in `<code>`. Everything is a React
 * text node, never HTML; the backticks of a code piece are dropped, those of
 * an unbalanced one are kept.
 */
export function TextWithCode({ text, className }: TextWithCodeProps) {
  return (
    <>
      {splitBackticks(text).map((part, index) =>
        part.code ? (
          <code
            key={index}
            className={cn(
              "bg-surface-sunken rounded-sm px-1 font-mono text-[0.92em] whitespace-nowrap",
              className,
            )}
          >
            {part.value}
          </code>
        ) : (
          <Fragment key={index}>{part.value}</Fragment>
        ),
      )}
    </>
  );
}
