// Long machine text (an error, a halt reason) cut to what a row can hold, with
// the whole text kept for a disclosure. Pure: callers render both parts.

export interface Digest {
  /** The first sentence or line, at most `max` characters. */
  readonly head: string;
  /** True when `head` is not the whole text. */
  readonly truncated: boolean;
}

/**
 * The first line of `text`, ended at its first sentence break when that comes
 * sooner, and cut at `max` characters on a word boundary with an ellipsis. A
 * text that already fits is returned as it is.
 */
export function digestText(text: string, max = 160): Digest {
  const trimmed = text.trim();
  const firstLine = trimmed.split("\n", 1)[0] ?? "";
  const sentence = /^(.*?[.!?])(\s|$)/.exec(firstLine);
  let head = sentence?.[1] ?? firstLine;
  if (head.length > max) {
    const cut = head.slice(0, max);
    const space = cut.lastIndexOf(" ");
    head = `${(space > max / 2 ? cut.slice(0, space) : cut).trimEnd()}…`;
  }
  return { head, truncated: head !== trimmed };
}
