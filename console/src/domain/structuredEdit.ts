// Edits to a spec's acceptance criteria and a ticket's header lines and
// covered-criteria list, as pure functions from the file's text to its new
// text. Each rewrites only the lines it names: every other line of the
// document comes back byte for byte, and an edit that changes nothing returns
// the text it was given. The result is ordinary file text: it is saved through
// the same route, against the same base hash, and validated by the server like
// any hand edit.
import {
  CRITERION_ITEM,
  criteriaSectionRange,
  forEachTopLevelLine,
  headingPositions,
  isIndented,
  requiredTicketHeadings,
  trimSpace,
} from "@/domain/specSkeleton";

/** One numbered criterion as an editor shows it. */
export interface CriterionItem {
  /** Its text without the "N. " prefix; continuation lines as written, one per line. */
  readonly body: string;
}

/** One ticket plan list item, without its marker; continuation lines stay as written. */
export interface ListItem {
  readonly body: string;
}

interface Span {
  readonly first: number;
  readonly last: number;
}

interface CriteriaLayout {
  readonly lines: string[];
  /** The section body, [start, end). */
  readonly start: number;
  readonly end: number;
  /** Each criterion's first and last line, interior blank lines included. */
  readonly spans: readonly Span[];
  /** "\r" when the document's lines end in CRLF. */
  readonly eol: string;
}

interface ListLayout {
  readonly lines: string[];
  readonly start: number;
  readonly end: number;
  readonly spans: readonly Span[];
  readonly eol: string;
}

const PREFIX = /^([0-9]+)([.)])([\t\n\f\r ]+)/;

function stripEol(line: string): string {
  return line.endsWith("\r") ? line.slice(0, -1) : line;
}

/**
 * `replacement` for the lines up to `lastIndex`, ending as the file did when
 * that is the file's last line: text after the final line break carries no
 * CR, and one written there would be a byte nobody typed.
 */
function endingAsTheFileDid(
  replacement: readonly string[],
  lines: readonly string[],
  lastIndex: number,
): string[] {
  const out = [...replacement];
  if (lastIndex !== lines.length - 1 || out.length === 0) return out;
  const cr = (lines[lastIndex] ?? "").endsWith("\r") ? "\r" : "";
  out[out.length - 1] = stripEol(out[out.length - 1] ?? "") + cr;
  return out;
}

function layout(
  text: string,
  range: { readonly start: number; readonly end: number } | null,
  itemPattern: RegExp,
): ListLayout | null {
  const lines = text.split("\n");
  if (range === null) return null;
  const spans: Span[] = [];
  let current: { first: number; last: number } | null = null;
  for (let i = range.start; i < range.end; i++) {
    const raw = lines[i] ?? "";
    const trimmed = trimSpace(raw);
    if (trimmed === "") continue;
    if (!isIndented(raw) && itemPattern.test(trimmed)) {
      if (current !== null) spans.push(current);
      current = { first: i, last: i };
    } else if (current !== null) {
      current.last = i;
    }
  }
  if (current !== null) spans.push(current);
  const eol = lines.slice(0, -1).some((line) => line.endsWith("\r")) ? "\r" : "";
  return { lines, ...range, spans, eol };
}

function criteriaLayout(text: string): CriteriaLayout | null {
  return layout(text, criteriaSectionRange(text.split("\n")), CRITERION_ITEM);
}

function bodyOf(lines: readonly string[], span: Span, pattern = PREFIX): string {
  const own = lines.slice(span.first, span.last + 1).map(stripEol);
  own[0] = (own[0] ?? "").replace(pattern, "");
  return own.join("\n");
}

/** The spec's numbered criteria, in order; null when it has no "## Acceptance criteria" heading. */
export function criteriaItems(text: string): CriterionItem[] | null {
  const doc = criteriaLayout(text);
  if (doc === null) return null;
  return doc.spans.map((span) => ({ body: bodyOf(doc.lines, span) }));
}

/** The first line's own prefix ("3) "), or a fresh one in the list's style. */
function prefixFor(doc: ListLayout, span: Span | null, number: number, pattern = PREFIX): string {
  const model = pattern.exec(doc.lines[(span ?? doc.spans[0])?.first ?? -1] ?? "");
  const delimiter = model?.[2] ?? ".";
  if (span === null) return `${number}${delimiter} `;
  return `${model?.[1] ?? String(number)}${delimiter}${stripEol(model?.[3] ?? " ")}`;
}

/**
 * The lines of one criterion from an editor's body. A later line that would
 * read as a new criterion (unindented "2. ...") is indented so it stays a
 * continuation of this one.
 */
function itemLines(
  prefix: string,
  body: string,
  eol: string,
  itemPattern = CRITERION_ITEM,
): string[] {
  // Blank lines after an item's last text belong to the list's spacing, not
  // to the item: written here they would stay behind on the next edit.
  const own = body.split("\n");
  while (own.length > 1 && trimSpace(own[own.length - 1] ?? "") === "") own.pop();
  return own.map((line, i) => {
    if (i === 0) return prefix + line + eol;
    const opens = !isIndented(line) && itemPattern.test(trimSpace(line));
    return (opens ? "   " : "") + line + eol;
  });
}

/** Rewrites each criterion's leading number to its position, keeping its delimiter and spacing. */
function renumber(text: string): string {
  const doc = criteriaLayout(text);
  if (doc === null) return text;
  doc.spans.forEach((span, i) => {
    const line = doc.lines[span.first] ?? "";
    doc.lines[span.first] = line.replace(/^[0-9]+/, String(i + 1));
  });
  return doc.lines.join("\n");
}

/** Replaces criterion `index`'s text. Other lines, and its own number, are untouched. */
export function setCriterionBody(text: string, index: number, body: string): string {
  const doc = criteriaLayout(text);
  const span = doc?.spans[index];
  if (doc === null || span === undefined) return text;
  if (body === bodyOf(doc.lines, span)) return text;
  const replacement = endingAsTheFileDid(
    itemLines(prefixFor(doc, span, index + 1), body, doc.eol),
    doc.lines,
    span.last,
  );
  doc.lines.splice(span.first, span.last - span.first + 1, ...replacement);
  return doc.lines.join("\n");
}

/** Appends a criterion after the last one, spaced as the list already is, and renumbers. */
export function addCriterion(text: string, body: string): string {
  const doc = criteriaLayout(text);
  if (doc === null) return text;
  const { lines, spans, eol } = doc;
  const added = itemLines(prefixFor(doc, null, spans.length + 1), body, eol);
  const last = spans.at(-1);
  if (last === undefined) {
    // An empty section: after its last non-blank line, set off by blank lines.
    let at = doc.end;
    while (at > doc.start && trimSpace(lines[at - 1] ?? "") === "") at--;
    const before = trimSpace(lines[at - 1] ?? "") === "" ? [] : [eol];
    const after = at < lines.length && trimSpace(lines[at] ?? "") !== "" ? [eol] : [];
    lines.splice(at, 0, ...before, ...added, ...after);
    return lines.join("\n");
  }
  const first = spans[0];
  const second = spans[1];
  const gap =
    first !== undefined && second !== undefined ? lines.slice(first.last + 1, second.first) : [];
  // A last line with no newline after it gains one before the new item.
  if (last.last === lines.length - 1) lines[last.last] = stripEol(lines[last.last] ?? "") + eol;
  const tail = last.last === lines.length - 1 ? added.map(stripEol) : added;
  lines.splice(last.last + 1, 0, ...gap, ...tail);
  return renumber(lines.join("\n"));
}

/** Removes criterion `index` with the blank lines that set it off, and renumbers. */
export function removeCriterion(text: string, index: number): string {
  const doc = criteriaLayout(text);
  const span = doc?.spans[index];
  if (doc === null || span === undefined) return text;
  const next = doc.spans[index + 1];
  const previous = doc.spans[index - 1];
  const from = next === undefined && previous !== undefined ? previous.last + 1 : span.first;
  const to = next === undefined ? span.last + 1 : next.first;
  doc.lines.splice(from, to - from);
  return renumber(doc.lines.join("\n"));
}

/** Swaps criterion `index` with its neighbour `offset` away (-1 up, 1 down), and renumbers. */
export function moveCriterion(text: string, index: number, offset: -1 | 1): string {
  const doc = criteriaLayout(text);
  const upper = doc?.spans[Math.min(index, index + offset)];
  const lower = doc?.spans[Math.max(index, index + offset)];
  if (doc === null || upper === undefined || lower === undefined || upper === lower) return text;
  const { lines } = doc;
  const swapped = [
    ...lines.slice(lower.first, lower.last + 1),
    ...lines.slice(upper.last + 1, lower.first),
    ...lines.slice(upper.first, upper.last + 1),
  ];
  // The document's last line carries no line ending; keep that true of whichever line lands there.
  if (lower.last === lines.length - 1 && doc.eol !== "") {
    const end = swapped.length - 1;
    const moved = lower.last - lower.first;
    swapped[moved] = stripEol(swapped[moved] ?? "") + doc.eol;
    swapped[end] = stripEol(swapped[end] ?? "");
  }
  lines.splice(upper.first, lower.last - upper.first + 1, ...swapped);
  return renumber(lines.join("\n"));
}

const BULLET_PREFIX = /^([-*])([\t\n\f\r ]+)/;
/** A trimmed line that opens a bullet item. */
const BULLET_ITEM = /^[-*][\t\n\f\r ]+[^\t\n\f\r ]/;
type SectionListHeading = "### Steps" | "### Files to touch";

function sectionListLayout(text: string, heading: SectionListHeading): ListLayout | null {
  const lines = text.split("\n");
  const positions = headingPositions(lines, requiredTicketHeadings);
  const at = requiredTicketHeadings.indexOf(heading);
  const headingPosition = positions[at];
  if (headingPosition === undefined || headingPosition === -1) return null;
  const next = positions.slice(at + 1).find((position) => position !== -1);
  const range = { start: headingPosition + 1, end: next ?? lines.length };
  return layout(text, range, heading === "### Steps" ? CRITERION_ITEM : BULLET_ITEM);
}

function sectionPrefix(
  doc: ListLayout,
  heading: SectionListHeading,
  span: Span | null,
  number: number,
): string {
  if (heading === "### Files to touch") {
    const model = BULLET_PREFIX.exec(doc.lines[(span ?? doc.spans[0])?.first ?? -1] ?? "");
    return `${model?.[1] ?? "-"}${stripEol(model?.[2] ?? " ")}`;
  }
  return prefixFor(doc, span, number, PREFIX);
}

function renumberSection(text: string, heading: SectionListHeading): string {
  if (heading !== "### Steps") return text;
  const doc = sectionListLayout(text, heading);
  if (doc === null) return text;
  doc.spans.forEach((span, i) => {
    const line = doc.lines[span.first] ?? "";
    doc.lines[span.first] = line.replace(/^[0-9]+/, String(i + 1));
  });
  return doc.lines.join("\n");
}

/** The items in one ticket plan section; null when its heading is absent. */
export function sectionListItems(text: string, heading: SectionListHeading): ListItem[] | null {
  const doc = sectionListLayout(text, heading);
  if (doc === null) return null;
  const pattern = heading === "### Steps" ? PREFIX : BULLET_PREFIX;
  return doc.spans.map((span) => ({ body: bodyOf(doc.lines, span, pattern) }));
}

/** Replaces one ticket plan list item, preserving its marker and surrounding document bytes. */
export function setSectionListItem(
  text: string,
  heading: SectionListHeading,
  index: number,
  body: string,
): string {
  const doc = sectionListLayout(text, heading);
  const span = doc?.spans[index];
  if (doc === null || span === undefined) return text;
  const pattern = heading === "### Steps" ? PREFIX : BULLET_PREFIX;
  if (body === bodyOf(doc.lines, span, pattern)) return text;
  const replacement = endingAsTheFileDid(
    itemLines(
      sectionPrefix(doc, heading, span, index + 1),
      body,
      doc.eol,
      heading === "### Steps" ? CRITERION_ITEM : BULLET_ITEM,
    ),
    doc.lines,
    span.last,
  );
  doc.lines.splice(span.first, span.last - span.first + 1, ...replacement);
  return doc.lines.join("\n");
}

/** Appends one item to a ticket plan section, preserving its list style. */
export function addSectionListItem(
  text: string,
  heading: SectionListHeading,
  body: string,
): string {
  const doc = sectionListLayout(text, heading);
  if (doc === null) return text;
  const itemPattern = heading === "### Steps" ? CRITERION_ITEM : BULLET_ITEM;
  const added = itemLines(
    sectionPrefix(doc, heading, null, doc.spans.length + 1),
    body,
    doc.eol,
    itemPattern,
  );
  const last = doc.spans.at(-1);
  if (last === undefined) {
    let at = doc.end;
    while (at > doc.start && trimSpace(doc.lines[at - 1] ?? "") === "") at--;
    const before = trimSpace(doc.lines[at - 1] ?? "") === "" ? [] : [doc.eol];
    const after = at < doc.lines.length && trimSpace(doc.lines[at] ?? "") !== "" ? [doc.eol] : [];
    doc.lines.splice(at, 0, ...before, ...added, ...after);
    return doc.lines.join("\n");
  }
  const first = doc.spans[0];
  const second = doc.spans[1];
  const gap =
    first !== undefined && second !== undefined
      ? doc.lines.slice(first.last + 1, second.first)
      : [];
  if (last.last === doc.lines.length - 1) {
    doc.lines[last.last] = stripEol(doc.lines[last.last] ?? "") + doc.eol;
  }
  const tail = last.last === doc.lines.length - 1 ? added.map(stripEol) : added;
  doc.lines.splice(last.last + 1, 0, ...gap, ...tail);
  return renumberSection(doc.lines.join("\n"), heading);
}

/** Removes one ticket plan list item and renumbers numbered steps. */
export function removeSectionListItem(
  text: string,
  heading: SectionListHeading,
  index: number,
): string {
  const doc = sectionListLayout(text, heading);
  const span = doc?.spans[index];
  if (doc === null || span === undefined) return text;
  const next = doc.spans[index + 1];
  const previous = doc.spans[index - 1];
  const from = next === undefined && previous !== undefined ? previous.last + 1 : span.first;
  const to = next === undefined ? span.last + 1 : next.first;
  doc.lines.splice(from, to - from);
  return renumberSection(doc.lines.join("\n"), heading);
}

/** Swaps neighbouring ticket plan list items and renumbers numbered steps. */
export function moveSectionListItem(
  text: string,
  heading: SectionListHeading,
  index: number,
  offset: -1 | 1,
): string {
  const doc = sectionListLayout(text, heading);
  const upper = doc?.spans[Math.min(index, index + offset)];
  const lower = doc?.spans[Math.max(index, index + offset)];
  if (doc === null || upper === undefined || lower === undefined || upper === lower) return text;
  const { lines } = doc;
  const swapped = [
    ...lines.slice(lower.first, lower.last + 1),
    ...lines.slice(upper.last + 1, lower.first),
    ...lines.slice(upper.first, upper.last + 1),
  ];
  if (lower.last === lines.length - 1 && doc.eol !== "") {
    const end = swapped.length - 1;
    const moved = lower.last - lower.first;
    swapped[moved] = stripEol(swapped[moved] ?? "") + doc.eol;
    swapped[end] = stripEol(swapped[end] ?? "");
  }
  lines.splice(upper.first, lower.last - upper.first + 1, ...swapped);
  return renumberSection(lines.join("\n"), heading);
}

function headerLine(lines: readonly string[], key: string): number {
  let at = -1;
  forEachTopLevelLine(lines, (trimmed, index) => {
    if (at === -1 && trimmed.startsWith(key)) at = index;
  });
  return at;
}

/** The value of a ticket's `key` header line ("Allowed-Files:"), trimmed; null when the line is absent. */
export function ticketHeaderValue(text: string, key: string): string | null {
  const lines = text.split("\n");
  const at = headerLine(lines, key);
  return at === -1 ? null : trimSpace(trimSpace(lines[at] ?? "").slice(key.length));
}

/**
 * Sets a ticket's `key` header line to `value`. An existing line is rewritten
 * in place; a missing one is added after the last of `keys` present, or as the
 * first line.
 */
export function setTicketHeaderValue(
  text: string,
  key: string,
  value: string,
  keys: readonly string[],
): string {
  if (ticketHeaderValue(text, key) === value) return text;
  const lines = text.split("\n");
  const eol = lines.slice(0, -1).some((line) => line.endsWith("\r")) ? "\r" : "";
  const line = value === "" ? key : `${key} ${value}`;
  const at = headerLine(lines, key);
  if (at !== -1) {
    lines[at] = line + (lines[at]?.endsWith("\r") === true ? "\r" : "");
    return lines.join("\n");
  }
  const after = Math.max(-1, ...keys.map((other) => headerLine(lines, other)));
  lines.splice(after + 1, 0, line + eol);
  return lines.join("\n");
}

/** A comma-separated header value as its entries, trimmed, empties dropped. */
export function headerList(value: string): string[] {
  return value
    .split(",")
    .map(trimSpace)
    .filter((entry) => entry !== "");
}

const COVERED = "### Acceptance criteria covered";

/**
 * Rewrites the body of a ticket's "### Acceptance criteria covered" section
 * as one "- N" line per number, keeping the blank lines around it. Returns
 * the text unchanged when the ticket lacks the plan headings: there is no
 * section to write to.
 */
export function setCoveredCriteria(text: string, numbers: readonly number[]): string {
  const lines = text.split("\n");
  const positions = headingPositions(lines, requiredTicketHeadings);
  if (positions.includes(-1)) return text;
  const at = requiredTicketHeadings.indexOf(COVERED);
  const start = (positions[at] ?? 0) + 1;
  const end = positions[at + 1] ?? lines.length;
  let from = start;
  while (from < end && trimSpace(lines[from] ?? "") === "") from++;
  let to = end;
  while (to > from && trimSpace(lines[to - 1] ?? "") === "") to--;
  const eol = lines.slice(0, -1).some((line) => line.endsWith("\r")) ? "\r" : "";
  const items = numbers.map((n) => `- ${n}${eol}`);
  if (from === to) {
    // Nothing but blank lines: write after the first of them, or straight under the heading.
    const blank = start < end ? 1 : 0;
    lines.splice(start + blank, 0, ...items, ...(blank === 0 && items.length > 0 ? [eol] : []));
    return lines.join("\n");
  }
  lines.splice(from, to - from, ...items);
  return lines.join("\n");
}

/** One required section of a file as an editor shows it. */
export interface SectionSlice {
  readonly heading: string;
  /** The section's body lines joined with "\n" (see `sections`); "" when absent or empty. */
  readonly body: string;
  /** Whether the heading was found, in order. */
  readonly present: boolean;
}

interface SectionSpan {
  /** Index of the heading line. */
  readonly at: number;
  /** First body line, and one past the last body line. */
  readonly start: number;
  readonly end: number;
}

function sectionSpan(lines: readonly string[], headings: readonly string[], heading: string) {
  const index = headings.indexOf(heading);
  if (index === -1) return null;
  const positions = headingPositions(lines, headings);
  const at = positions[index] ?? -1;
  if (at === -1) return null;
  const next = positions.slice(index + 1).find((p) => p !== -1) ?? lines.length;
  // Blank lines (and the empty piece after a final newline) just before the
  // next heading or the end separate the sections: they belong to no body.
  let end = next;
  while (end > at + 1 && trimSpace(lines[end - 1] ?? "") === "") end--;
  const span: SectionSpan = { at, start: at + 1, end };
  return span;
}

/**
 * The body of each required heading: the lines after its heading line up to
 * the next required heading found (or the end of the file), without the blank
 * lines that end that stretch. Leading blank lines, interior blank lines and
 * every line's own bytes (a CR, trailing spaces) are kept. A container's body
 * ("## Plan") ends at its first sub-heading. A heading not found is absent.
 */
export function sections(text: string, headings: readonly string[]): SectionSlice[] {
  const lines = text.split("\n");
  return headings.map((heading) => {
    const span = sectionSpan(lines, headings, heading);
    if (span === null) return { heading, body: "", present: false };
    return { heading, body: lines.slice(span.start, span.end).join("\n"), present: true };
  });
}

function withoutCr(body: string): string {
  return body.replace(/\r(?=\n|$)/g, "");
}

/**
 * `text` with one section's body replaced and every other line untouched.
 * Only the body lines (see `sections`) are replaced; the blank lines after
 * them stay, so `setSectionBody(t, h, x, sections(t, h)[i].body) === t`.
 * Within the body, lines the new text keeps at its start and at its end come
 * back with their own bytes (a CR included), so a document of mixed line
 * endings changes only where the operator typed; a new line takes the
 * heading line's ending. Trailing blank lines of `body` are not written
 * (they are the spacing before the next heading, which stays). An empty
 * `body` removes the body lines. An absent heading returns `text`.
 */
export function setSectionBody(
  text: string,
  headings: readonly string[],
  heading: string,
  body: string,
): string {
  const lines = text.split("\n");
  const span = sectionSpan(lines, headings, heading);
  if (span === null) return text;
  const old = lines.slice(span.start, span.end);
  const next = withoutCr(body).split("\n");
  while (next.length > 0 && trimSpace(next[next.length - 1] ?? "") === "") next.pop();
  const same = (a: string | undefined, b: string | undefined) => stripEol(a ?? "") === b;
  if (old.length === next.length && old.every((line, i) => same(line, next[i]))) return text;
  let head = 0;
  while (head < old.length && head < next.length && same(old[head], next[head])) head++;
  let tail = 0;
  while (
    tail < old.length - head &&
    tail < next.length - head &&
    same(old[old.length - 1 - tail], next[next.length - 1 - tail])
  ) {
    tail++;
  }
  const headingLine = lines[span.at] ?? "";
  // A heading that is the file's last line has no line break of its own to
  // copy: the lines before it say what the document uses.
  const crlf =
    span.at === lines.length - 1
      ? lines.slice(0, -1).some((line) => line.endsWith("\r"))
      : headingLine.endsWith("\r");
  const eol = crlf ? "\r" : "";
  // The file's last line has no line break; kept as a line that others now
  // follow, it needs the one its neighbours have.
  const endsFile = span.end === lines.length && old.length > 0;
  const lastHadCr = (old[old.length - 1] ?? "").endsWith("\r");
  if (endsFile) old[old.length - 1] = stripEol(old[old.length - 1] ?? "") + eol;
  const written = [
    ...old.slice(0, head),
    ...next.slice(head, next.length - tail).map((line) => line + eol),
    ...old.slice(old.length - tail),
  ];
  const before = lines.slice(0, span.start);
  if (span.at === lines.length - 1 && written.length > 0) {
    // The heading now has lines after it, and the new last line ends the file.
    before[span.at] = headingLine + eol;
    written[written.length - 1] = stripEol(written[written.length - 1] ?? "");
    return [...before, ...written].join("\n");
  }
  if (endsFile && written.length > 0) {
    const last = written.length - 1;
    written[last] = stripEol(written[last] ?? "") + (lastHadCr ? "\r" : "");
  }
  return [...before, ...written, ...lines.slice(span.end)].join("\n");
}

const ACCEPTANCE_CRITERIA_HEADING = "## Acceptance criteria";

/**
 * Why `body` cannot be the body of `heading`, or null. A line equal to a
 * required heading changes the file's structure; under the acceptance
 * criteria, any "## " line does, because the criteria end at the first one.
 */
export function sectionBodyProblem(
  headings: readonly string[],
  body: string,
  heading = "",
): string | null {
  const lines = body.split("\n").map(trimSpace);
  const clash = lines.find((line) => headings.includes(line));
  if (clash !== undefined) {
    return `A line reading "${clash}" would add or move a section heading; edit the whole file to do that.`;
  }
  if (heading === ACCEPTANCE_CRITERIA_HEADING && lines.some((line) => line.startsWith("## "))) {
    return 'A line starting "## " ends the acceptance criteria there, so the criteria after it would not be read; edit the whole file to add a section.';
  }
  return null;
}
