// The structural checks the server runs on a saved spec or ticket, mirrored so
// an editor can show them while the operator types. The server stays the
// authority: it validates the saved bytes again on save and on approve. The Go
// originals are internal/request (ValidateSpecSkeleton, SpecAcceptanceCriteria,
// ValidateTicketPlan, TicketCoveredCriteria) and the header-line half of
// internal/policy's TicketStructureBrownfield; test/fixtures/vectors/
// spec-skeleton.json is run by both suites, so the two cannot drift unnoticed.

export const requiredSpecHeadings: readonly string[] = [
  "# Spec",
  "## Problem",
  "## Scope",
  "## Non-goals",
  "## Affected services and packages",
  "## Acceptance criteria",
  "## Risks",
  "## Open questions",
];

export const requiredTicketHeadings: readonly string[] = [
  "## Goal",
  "## Plan",
  "### Files to touch",
  "### Steps",
  "### Tests to add",
  "### Acceptance criteria covered",
  "## Out of scope",
];

export const requiredTicketHeaderKeys: readonly string[] = [
  "Verify-Command:",
  "Allowed-Files:",
  "Required-Changed-Files:",
];

const ACCEPTANCE_CRITERIA = "## Acceptance criteria";
const CRITERIA_COVERED = "### Acceptance criteria covered";
const PLAN_CONTAINER = "## Plan";

// Go's unicode.IsSpace set. String.prototype.trim differs from it (it strips
// U+FEFF and keeps U+0085), so the trim is written out.
const GO_SPACE =
  "\\t\\n\\v\\f\\r \\u0085\\u00a0\\u1680\\u2000-\\u200a\\u2028\\u2029\\u202f\\u205f\\u3000";
const GO_TRIM = new RegExp(`^[${GO_SPACE}]+|[${GO_SPACE}]+$`, "g");

function trimSpace(s: string): string {
  return s.replace(GO_TRIM, "");
}

function isIndented(raw: string): boolean {
  return raw.startsWith(" ") || raw.startsWith("\t");
}

// Go's regexp `\s`, `\S` and `\d` are ASCII-only; JavaScript's are not.
const CRITERION_ITEM = /^[0-9]+[.)][\t\n\f\r ]+[^\t\n\f\r ]/;
const COVERED_ITEM = /^(?:[-*][\t\n\f\r ]*)?([0-9]+)[\t\n\f\r ]*$/;
const GO_MAX_INT = 9223372036854775807n;

/** Line indexes of `headings` found in order, each searched after the one before; -1 where not found. */
function headingPositions(lines: readonly string[], headings: readonly string[]): number[] {
  const positions: number[] = [];
  let searchFrom = 0;
  for (const heading of headings) {
    let found = -1;
    for (let j = searchFrom; j < lines.length; j++) {
      if (trimSpace(lines[j] ?? "") === heading) {
        found = j;
        break;
      }
    }
    positions.push(found);
    if (found !== -1) searchFrom = found + 1;
  }
  return positions;
}

function anyNonBlank(lines: readonly string[]): boolean {
  return lines.some((line) => trimSpace(line) !== "");
}

/** What `ValidateSpecSkeleton` refuses, or both null/false when it accepts. */
export interface SpecSkeletonResult {
  /** The first required heading not found in order; null when all are. */
  readonly missingHeading: string | null;
  /** Every heading is there, and nothing but blank lines sits under "## Acceptance criteria". */
  readonly criteriaSectionEmpty: boolean;
}

export function validateSpecSkeleton(content: string): SpecSkeletonResult {
  const lines = content.split("\n");
  const positions = headingPositions(lines, requiredSpecHeadings);
  const missing = positions.indexOf(-1);
  if (missing !== -1) {
    return { missingHeading: requiredSpecHeadings[missing] ?? null, criteriaSectionEmpty: false };
  }
  const at = requiredSpecHeadings.indexOf(ACCEPTANCE_CRITERIA);
  const body = lines.slice((positions[at] ?? 0) + 1, positions[at + 1] ?? lines.length);
  return { missingHeading: null, criteriaSectionEmpty: !anyNonBlank(body) };
}

/** One numbered criterion: its source lines verbatim and the one-line text the server passes on. */
export interface CriterionBlock {
  readonly rawLines: readonly string[];
  readonly folded: string;
}

/**
 * Where one criterion ends and the next begins (`splitAcceptanceCriteriaBlocks`):
 * a criterion starts at an unindented "N." or "N)" line, and every following
 * non-blank line up to the next one belongs to it. Lines before the first
 * criterion are dropped.
 */
export function splitCriteriaBlocks(sectionLines: readonly string[]): CriterionBlock[] {
  const blocks: CriterionBlock[] = [];
  let current: string[] | null = null;
  const flush = () => {
    if (current !== null && current.length > 0) {
      blocks.push({ rawLines: current, folded: current.map(trimSpace).join(" ") });
    }
  };
  for (const raw of sectionLines) {
    const trimmed = trimSpace(raw);
    if (trimmed === "") continue;
    if (!isIndented(raw) && CRITERION_ITEM.test(trimmed)) {
      flush();
      current = [];
    }
    if (current !== null) current.push(raw);
  }
  flush();
  return blocks;
}

/** The [start, end) lines under the first "## Acceptance criteria", up to the next "## " line; null without the heading. */
export function criteriaSectionRange(
  lines: readonly string[],
): { readonly start: number; readonly end: number } | null {
  const heading = lines.findIndex((line) => trimSpace(line) === ACCEPTANCE_CRITERIA);
  if (heading === -1) return null;
  const start = heading + 1;
  let end = lines.length;
  for (let i = start; i < lines.length; i++) {
    if (trimSpace(lines[i] ?? "").startsWith("## ")) {
      end = i;
      break;
    }
  }
  return { start, end };
}

/** The spec's numbered criteria as the server reads them ("1. It works."), in order; empty where the server would refuse. */
export function specAcceptanceCriteria(content: string): string[] {
  const lines = content.split("\n");
  const range = criteriaSectionRange(lines);
  if (range === null) return [];
  return splitCriteriaBlocks(lines.slice(range.start, range.end)).map((b) => b.folded);
}

/** The numbers a "### Acceptance criteria covered" body names, or the first line that is not one. */
export interface CoveredCriteria {
  readonly numbers: readonly number[];
  /** The trimmed line the server would refuse; null when every line parses. */
  readonly badLine: string | null;
}

function parseCriteriaNumbers(lines: readonly string[]): CoveredCriteria {
  const numbers: number[] = [];
  for (const raw of lines) {
    const line = trimSpace(raw);
    if (line === "") continue;
    const digits = COVERED_ITEM.exec(line)?.[1];
    // Go's strconv.Atoi refuses a value past its int; one between 2^53 and
    // that limit is accepted there and only approximated here.
    if (digits === undefined || BigInt(digits) > GO_MAX_INT) return { numbers: [], badLine: line };
    numbers.push(Number(digits));
  }
  return { numbers, badLine: null };
}

/** What `ValidateTicketPlan` refuses; `ok` when it accepts. */
export interface TicketPlanResult {
  readonly ok: boolean;
  readonly missingHeading: string | null;
  /** The first heading with nothing but blank lines under it ("## Plan" is a container and exempt). */
  readonly emptySection: string | null;
  /** The covered-criteria line that is not "- N", or "" when the section names no number. */
  readonly badCriteriaLine: string | null;
  /** The criteria the ticket claims, in the order written; empty unless the headings are all there and the list parses. */
  readonly covered: readonly number[];
}

export function validateTicketPlan(content: string): TicketPlanResult {
  const lines = content.split("\n");
  const positions = headingPositions(lines, requiredTicketHeadings);
  const refused = { ok: false, missingHeading: null, emptySection: null, badCriteriaLine: null };
  const missing = positions.indexOf(-1);
  if (missing !== -1) {
    return { ...refused, missingHeading: requiredTicketHeadings[missing] ?? null, covered: [] };
  }
  const section = (i: number) =>
    lines.slice((positions[i] ?? 0) + 1, positions[i + 1] ?? lines.length);
  const parsed = parseCriteriaNumbers(section(requiredTicketHeadings.indexOf(CRITERIA_COVERED)));
  const covered = parsed.badLine === null ? parsed.numbers : [];
  const empty = requiredTicketHeadings.find(
    (heading, i) => heading !== PLAN_CONTAINER && !anyNonBlank(section(i)),
  );
  if (empty !== undefined) return { ...refused, emptySection: empty, covered };
  if (parsed.badLine !== null) return { ...refused, badCriteriaLine: parsed.badLine, covered };
  if (covered.length === 0) return { ...refused, badCriteriaLine: "", covered };
  return { ...refused, ok: true, covered };
}

function fenceMarker(trimmed: string): { readonly ch: string; readonly length: number } | null {
  const ch = trimmed.charAt(0);
  if (ch !== "`" && ch !== "~") return null;
  let length = 0;
  while (trimmed.charAt(length) === ch) length++;
  return length < 3 ? null : { ch, length };
}

function closesFence(trimmed: string, ch: string, minLength: number): boolean {
  let length = 0;
  while (trimmed.charAt(length) === ch) length++;
  return length >= minLength && trimSpace(trimmed.slice(length)) === "";
}

/**
 * The required header lines a ticket lacks: each must start an unindented
 * line outside a fenced code block (`TicketStructureBrownfield`'s header
 * check; its section checks are the server's alone).
 */
export function missingTicketHeaderKeys(content: string): string[] {
  const found = new Set<string>();
  let fence: { readonly ch: string; readonly length: number } | null = null;
  for (const line of content.split("\n")) {
    const trimmed = trimSpace(line);
    if (fence !== null) {
      if (closesFence(trimmed, fence.ch, fence.length)) fence = null;
      continue;
    }
    fence = fenceMarker(trimmed);
    if (fence !== null || isIndented(line)) continue;
    for (const key of requiredTicketHeaderKeys) if (trimmed.startsWith(key)) found.add(key);
  }
  return requiredTicketHeaderKeys.filter((key) => !found.has(key));
}

/** One line of the checklist beside an editor. */
export interface StructureRow {
  readonly label: string;
  readonly ok: boolean;
  /** What is wrong, or what was read ("2 parsed"); "" when there is nothing to add. */
  readonly note: string;
}

export interface StructureCheck {
  readonly rows: readonly StructureRow[];
  /** Every mirrored check passes. The server may still refuse the save. */
  readonly passes: boolean;
}

/**
 * A heading row per required heading. One the server's in-order search would
 * not reach (a heading after a missing one) is still looked for from the last
 * found position, so the list shows everything absent at once.
 */
function headingRows(
  lines: readonly string[],
  headings: readonly string[],
  note: (heading: string, body: readonly string[]) => string,
): StructureRow[] {
  const positions = headingPositions(lines, headings);
  return headings.map((heading, i) => {
    const at = positions[i] ?? -1;
    if (at === -1) return { label: heading, ok: false, note: "missing, or out of order" };
    const next = positions.slice(i + 1).find((p) => p !== -1) ?? lines.length;
    const problem = note(heading, lines.slice(at + 1, next));
    return { label: heading, ok: problem === "", note: problem };
  });
}

export function specStructure(content: string): StructureCheck {
  const lines = content.split("\n");
  const criteria = specAcceptanceCriteria(content);
  const rows = headingRows(lines, requiredSpecHeadings, (heading, body) =>
    heading === ACCEPTANCE_CRITERIA && !anyNonBlank(body) ? "no content" : "",
  );
  rows.push({
    label: "Numbered criteria",
    ok: criteria.length > 0,
    note: criteria.length > 0 ? `${criteria.length} parsed` : 'none parsed (want "1. ...")',
  });
  return { rows, passes: rows.every((row) => row.ok) };
}

export function ticketStructure(content: string): StructureCheck {
  const lines = content.split("\n");
  const missingKeys = new Set(missingTicketHeaderKeys(content));
  const plan = validateTicketPlan(content);
  const rows: StructureRow[] = requiredTicketHeaderKeys.map((key) => ({
    label: key,
    ok: !missingKeys.has(key),
    note: missingKeys.has(key) ? "missing" : "",
  }));
  rows.push(
    ...headingRows(lines, requiredTicketHeadings, (heading, body) =>
      heading !== PLAN_CONTAINER && !anyNonBlank(body) ? "no content" : "",
    ),
  );
  const listed = plan.covered.length > 0;
  rows.push({
    label: "Criteria covered",
    ok: listed,
    note: listed
      ? plan.covered.join(", ")
      : plan.badCriteriaLine !== null && plan.badCriteriaLine !== ""
        ? `"${plan.badCriteriaLine}" is not "- N"`
        : 'none parsed (want "- N")',
  });
  return { rows, passes: plan.ok && missingKeys.size === 0 };
}
