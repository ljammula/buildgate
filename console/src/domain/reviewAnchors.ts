import { ticketRelPath } from "@/domain/contentHash";
import type { Rejection, RejectionAnchor, RequestSummary } from "@/domain/request";
import {
  CRITERION_ITEM,
  criteriaSectionRange,
  headingPositions,
  isIndented,
  requiredSpecHeadings,
  requiredTicketHeadings,
  sectionRange,
  specAcceptanceCriteria,
  trimSpace,
} from "@/domain/specSkeleton";

/** A place in a reviewed file a rejection note can be tied to. */
export interface AnchorTarget {
  /** Unique among a request's targets; the option's value. */
  readonly id: string;
  /** What the operator picks: "spec.md · Acceptance criteria · 2. A key is scoped…". */
  readonly label: string;
  readonly path: string;
  readonly section: string;
  readonly item: number;
}

const LABEL_LIMIT = 80;

function clip(text: string): string {
  return text.length <= LABEL_LIMIT ? text : `${text.slice(0, LABEL_LIMIT - 1)}…`;
}

function fileTargets(
  path: string,
  content: string,
  headings: readonly string[],
  items: (heading: string) => readonly string[],
): AnchorTarget[] {
  const targets: AnchorTarget[] = [
    { id: path, label: `${path} · the whole file`, path, section: "", item: 0 },
  ];
  const positions = headingPositions(content.split("\n"), headings);
  headings.forEach((heading, i) => {
    if (positions[i] === -1) return;
    const name = heading.replace(/^#+\s*/, "");
    targets.push({
      id: `${path}\n${heading}`,
      label: `${path} · ${name}`,
      path,
      section: heading,
      item: 0,
    });
    items(heading).forEach((text, n) => {
      targets.push({
        id: `${path}\n${heading}\n${n + 1}`,
        label: `${path} · ${name} · ${clip(text)}`,
        path,
        section: heading,
        item: n + 1,
      });
    });
  });
  return targets;
}

/**
 * The places a "Request changes" note can point at, for the documents the
 * request's state puts under review: spec.md's sections and each acceptance
 * criterion at spec_review; each ticket file's sections at plan_review.
 * Empty in any other state (an oracle review's files have no sections the
 * console reads).
 */
export function anchorTargets(request: RequestSummary): AnchorTarget[] {
  if (request.state === "spec_review" && request.spec !== "") {
    const criteria = specAcceptanceCriteria(request.spec);
    return fileTargets("spec.md", request.spec, requiredSpecHeadings, (heading) =>
      heading === "## Acceptance criteria" ? criteria : [],
    );
  }
  if (request.state === "plan_review") {
    return request.tickets.flatMap((ticket) => {
      const path = ticketRelPath(ticket.specPath);
      if (path === null || ticket.content === "") return [];
      return fileTargets(path, ticket.content, requiredTicketHeadings, () => []);
    });
  }
  return [];
}

/** Where a note points, for a reader: "spec.md · Acceptance criteria · number 2". */
export function anchorPlace(anchor: RejectionAnchor): string {
  const section = anchor.section.replace(/^#+\s*/, "");
  return [anchor.path, section, anchor.item > 0 ? `number ${anchor.item}` : ""]
    .filter((part) => part !== "")
    .join(" · ");
}

const ACCEPTANCE_CRITERIA = "## Acceptance criteria";

function headingsFor(path: string): readonly string[] | null {
  if (path === "spec.md") return requiredSpecHeadings;
  if (/^tickets\/[^/]+\.spec\.md$/.test(path)) return requiredTicketHeadings;
  return null;
}

/**
 * The text an anchor points at in one version of its file: the numbered
 * criterion, the section from its heading to the next required heading, or
 * the whole file. Null when that version has no such place (the heading or
 * the criterion is gone, or the file is not one the console reads sections
 * of). Trailing blank lines are dropped, so two versions of a section compare
 * equal when only the spacing before the next heading moved.
 */
export function anchorExcerpt(content: string, anchor: RejectionAnchor): string | null {
  if (anchor.section === "") return content;
  const headings = headingsFor(anchor.path);
  if (headings === null) return null;
  if (anchor.item > 0 && anchor.path === "spec.md" && anchor.section === ACCEPTANCE_CRITERIA) {
    return specAcceptanceCriteria(content)[anchor.item - 1] ?? null;
  }
  const lines = content.split("\n");
  const range = sectionRange(lines, headings, anchor.section);
  if (range === null) return null;
  let end = range.end;
  while (end > range.start + 1 && trimSpace(lines[end - 1] ?? "") === "") end--;
  return lines.slice(range.start, end).join("\n");
}

/** One anchored note with the text it pointed at when written and that place's text now. */
export interface AnchoredChange {
  readonly anchor: RejectionAnchor;
  readonly place: string;
  /** The place in the rejected revision; null when the revision has no such file or place. */
  readonly before: string | null;
  /** The same place in the current text; null when it is gone. */
  readonly after: string | null;
  /**
   * For a numbered criterion whose rejected text is in the current list
   * under another number: that number, and `after` is that criterion.
   * Criteria are matched by number otherwise, so a list that lost an item
   * above this one shows its neighbour: the screen says which it compares.
   */
  readonly movedTo: number | null;
}

/** A criterion's text without its "N. " prefix. */
function bare(criterion: string): string {
  return criterion.replace(/^[0-9]+[.)][\t\n\f\r ]+/, "");
}

/**
 * Each anchored note of a rejection against its place: what the operator
 * was looking at, and what is there now. `rejected` is the revision's files
 * by request-relative path; `current` reads the request's present text of a
 * path ("" for a file that no longer exists).
 */
export function anchoredChanges(
  rejection: Rejection,
  rejected: Readonly<Record<string, string>>,
  current: (path: string) => string,
): AnchoredChange[] {
  return rejection.anchors.map((anchor) => {
    const then = Object.hasOwn(rejected, anchor.path) ? (rejected[anchor.path] ?? "") : null;
    const now = current(anchor.path);
    const before = then === null ? null : anchorExcerpt(then, anchor);
    const after = now === "" ? null : anchorExcerpt(now, anchor);
    const numbered =
      anchor.item > 0 && anchor.path === "spec.md" && anchor.section === ACCEPTANCE_CRITERIA;
    if (numbered && before !== null && (after === null || bare(after) !== bare(before))) {
      const criteria = specAcceptanceCriteria(now);
      const at = criteria.findIndex((criterion) => bare(criterion) === bare(before));
      if (at !== -1) {
        return {
          anchor,
          place: anchorPlace(anchor),
          before,
          after: criteria[at] ?? null,
          movedTo: at + 1,
        };
      }
    }
    return { anchor, place: anchorPlace(anchor), before, after, movedTo: null };
  });
}

/**
 * The rejection a revision was snapshotted for: the server stamps both with
 * the same instant and operator. Null for a revision whose rejection the
 * request no longer lists.
 */
export function rejectionForRevision(
  rejections: readonly Rejection[],
  revision: { readonly at: string; readonly by: string },
): Rejection | null {
  const instant = Date.parse(revision.at);
  return rejections.find((r) => r.by === revision.by && Date.parse(r.at) === instant) ?? null;
}

const QUOTE_BYTE_LIMIT = 2000;
const QUOTE_SHOWN = 60;

/** A quote as shown beside its note: one line, clipped. */
export function clipQuote(quote: string): string {
  const oneLine = quote.split(/\s+/).join(" ").trim();
  return oneLine.length <= QUOTE_SHOWN ? oneLine : `${oneLine.slice(0, QUOTE_SHOWN - 1)}…`;
}

/** The longest prefix of text that fits in `limit` UTF-8 bytes, cut between characters. */
function clipBytes(text: string, limit: number): string {
  const encoder = new TextEncoder();
  let used = 0;
  let out = "";
  for (const ch of text) {
    used += encoder.encode(ch).length;
    if (used > limit) break;
    out += ch;
  }
  return out;
}

/** Where a selection of a reviewed file lands: the place and the passage, ready for a note. */
export interface SelectionAnchor {
  readonly path: string;
  readonly section: string;
  readonly item: number;
  readonly quote: string;
}

/**
 * The anchor for the characters [start, end) of `content`, the file at `path`
 * (request-relative): the nearest required heading above `start` (""
 * before the first), the numbered criterion `start` is in when that section is
 * the spec's acceptance criteria, and the selected text as the quote. The
 * quote is cut at the end of its section and at the server's byte limit, so a
 * selection that runs across a heading still names one section the quote is
 * really in. Null for an empty selection, a file the console reads no
 * sections of, or a range outside the content.
 */
export function anchorForSelection(
  path: string,
  content: string,
  start: number,
  end: number,
): SelectionAnchor | null {
  const headings = headingsFor(path);
  if (headings === null || start < 0 || end > content.length || start >= end) return null;
  const lines = content.split("\n");
  const positions = headingPositions(lines, headings);
  const lineStarts: number[] = [];
  let offset = 0;
  for (const line of lines) {
    lineStarts.push(offset);
    offset += line.length + 1;
  }
  // The line the selection starts in.
  let startLine = 0;
  while (startLine + 1 < lines.length && (lineStarts[startLine + 1] ?? Infinity) <= start) {
    startLine++;
  }
  let at = -1;
  positions.forEach((position, i) => {
    if (position !== -1 && position <= startLine) at = i;
  });
  const section = at === -1 ? "" : (headings[at] ?? "");
  const next = positions.slice(at + 1).find((position) => position !== -1);
  const sectionEnd = next === undefined ? content.length : (lineStarts[next] ?? content.length);
  const quote = clipBytes(content.slice(start, Math.min(end, sectionEnd)).trim(), QUOTE_BYTE_LIMIT);
  if (quote === "") return null;
  return { path, section, item: itemAt(path, section, lines, startLine), quote };
}

/** The 1-based numbered criterion line `line` is in under the spec's acceptance criteria; 0 elsewhere. */
function itemAt(path: string, section: string, lines: readonly string[], line: number): number {
  if (path !== "spec.md" || section !== ACCEPTANCE_CRITERIA) return 0;
  const range = criteriaSectionRange(lines);
  if (range === null || line < range.start || line >= range.end) return 0;
  let item = 0;
  for (let i = range.start; i <= line; i++) {
    const raw = lines[i] ?? "";
    if (!isIndented(raw) && CRITERION_ITEM.test(trimSpace(raw))) item++;
  }
  return item;
}

/**
 * Whether the redraft that follows a rejection with these notes and this
 * reason changes only the commented sections: the server's rule
 * (`RedraftScope`). Any free reason, a note on the whole of spec.md, or a note
 * on a file that is neither spec.md nor a ticket asks for the whole document.
 */
export function redraftIsScoped(notes: readonly RejectionAnchor[], reason: string): boolean {
  if (notes.length === 0 || reason.trim() !== "") return false;
  return notes.every(
    (n) => headingsFor(n.path) !== null && !(n.path === "spec.md" && n.section === ""),
  );
}
