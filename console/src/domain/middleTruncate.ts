/**
 * A long id or path with its middle replaced by an ellipsis, keeping the head
 * (which says which request) and the tail (the timestamp or counter that
 * tells siblings apart). A value within `max` characters is returned as is.
 * The result never exceeds `max` characters.
 */
export function middleTruncate(value: string, max: number): string {
  if (max < 5 || value.length <= max) return value;
  const keep = max - 1;
  const tail = Math.floor(keep / 2);
  const head = keep - tail;
  return `${value.slice(0, head)}…${value.slice(value.length - tail)}`;
}

/**
 * The first `keep` characters of a value, then `mark` when anything was cut:
 * for an id whose head is the part that identifies it (a hash prefix, a run
 * id's slug). Unlike middleTruncate the result can be `keep + mark.length`
 * long. A value within `keep` characters is returned as is.
 */
export function headTruncate(value: string, keep: number, mark = ""): string {
  return value.length > keep ? `${value.slice(0, keep)}${mark}` : value;
}

/**
 * A path as its last `keep` segments behind a leading "…/": the end of a path
 * says which project, the front only says whose home directory. A path with
 * no more than `keep` segments is returned as is.
 */
export function shortPath(path: string, keep = 2): string {
  const segments = path.split("/").filter((s) => s !== "");
  return segments.length > keep ? `…/${segments.slice(-keep).join("/")}` : path;
}
