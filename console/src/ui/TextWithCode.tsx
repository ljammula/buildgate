import { Fragment } from "react";

import { escapeInvisible, pastesAsShown } from "@/domain/textEscape";
import { cn } from "@/ui/cn";
import { CopyButton } from "@/ui/CopyButton";

export interface TextWithCodeProps {
  readonly text: string;
  readonly className?: string;
  /** A copy button after each code span; off where the caller already draws one for the same command. */
  readonly copy?: boolean;
  readonly writeText?: (text: string) => Promise<void>;
}

/** Code up to this long stays on one line with its copy button; a longer one wraps and the button follows its end. */
const shortCodeLength = 40;

function copyLabel(code: string): string {
  return `Copy ${code.length > shortCodeLength ? `${code.slice(0, shortCodeLength)}…` : code}`;
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
export function TextWithCode({ text, className, copy = true, writeText }: TextWithCodeProps) {
  return (
    <>
      {splitBackticks(text).map((part, index) => {
        if (!part.code) return <Fragment key={index}>{part.value}</Fragment>;
        const code = (
          <code
            className={cn(
              "bg-surface-sunken rounded-sm px-1 font-mono text-[0.92em] break-words",
              className,
            )}
          >
            {/* Server and agent text: a hidden character is drawn as its escape. */}
            {escapeInvisible(part.value)}
          </code>
        );
        // Copied only when the paste is exactly what is read here.
        if (!copy || !pastesAsShown(part.value)) return <Fragment key={index}>{code}</Fragment>;
        const button = (
          <CopyButton
            size="sm"
            text={part.value}
            label={copyLabel(part.value)}
            writeText={writeText}
            className="ml-0.5 align-middle"
          />
        );
        return part.value.length <= shortCodeLength ? (
          <span key={index} className="whitespace-nowrap">
            {code}
            {button}
          </span>
        ) : (
          <Fragment key={index}>
            {code}
            {button}
          </Fragment>
        );
      })}
    </>
  );
}
