/**
 * How much log text the pane keeps: a long-running build otherwise grows
 * both memory and the render cost without bound for as long as the tab
 * stays open, which would undermine a viewer that is off by default.
 * Keeping only the most recent slice is the right trade-off for a tail
 * viewer: the oldest output is also the least likely to still be relevant.
 */
export const maxLogBufferChars = 200_000;

/** `text` with `chunk` appended, cut from the front to the most recent `maxLogBufferChars`. */
export function appendLog(text: string, chunk: string): string {
  const combined = text + chunk;
  return combined.length > maxLogBufferChars
    ? combined.slice(combined.length - maxLogBufferChars)
    : combined;
}
