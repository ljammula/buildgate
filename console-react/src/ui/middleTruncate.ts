/**
 * A long id or path with its middle replaced by an ellipsis, keeping the head
 * (which says which request) and the tail (the timestamp or counter that
 * tells siblings apart). A value within `max` characters is returned as is.
 */
export function middleTruncate(value: string, max: number): string {
  if (max < 5 || value.length <= max) return value;
  const keep = max - 1;
  const tail = Math.floor(keep / 2);
  const head = keep - tail;
  return `${value.slice(0, head)}…${value.slice(value.length - tail)}`;
}
