import { ticketRelPath } from "@/domain/contentHash";
import type { Rejection, RejectionAnchor, RequestSummary } from "@/domain/request";
import {
  headingPositions,
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
    return {
      anchor,
      place: anchorPlace(anchor),
      before: then === null ? null : anchorExcerpt(then, anchor),
      after: now === "" ? null : anchorExcerpt(now, anchor),
    };
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
