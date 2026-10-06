import { ticketRelPath } from "@/domain/contentHash";
import type { RequestSummary } from "@/domain/request";
import {
  headingPositions,
  requiredSpecHeadings,
  requiredTicketHeadings,
  specAcceptanceCriteria,
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
