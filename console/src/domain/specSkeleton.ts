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
const OUT_OF_SCOPE = "## Out of scope";

/** The byte cap the server applies to an operator-shaped spec. */
export const MAX_HANDED_OVER_SPEC_BYTES = 120_000;

// Go's unicode.IsSpace set. String.prototype.trim differs from it (it strips
// U+FEFF and keeps U+0085), so the trim is written out.
const GO_SPACE =
  "\\t\\n\\v\\f\\r \\u0085\\u00a0\\u1680\\u2000-\\u200a\\u2028\\u2029\\u202f\\u205f\\u3000";
const GO_TRIM = new RegExp(`^[${GO_SPACE}]+|[${GO_SPACE}]+$`, "g");

/** Go's `strings.TrimSpace`. */
export function trimSpace(s: string): string {
  return s.replace(GO_TRIM, "");
}

/** Whether a raw line starts with a space or a tab: what makes a line a continuation, not a new item. */
export function isIndented(raw: string): boolean {
  return raw.startsWith(" ") || raw.startsWith("\t");
}

// Go's regexp `\s`, `\S` and `\d` are ASCII-only; JavaScript's are not.
/** A trimmed line that opens a numbered criterion (`specCriterionItemRE`). */
export const CRITERION_ITEM = /^[0-9]+[.)][\t\n\f\r ]+[^\t\n\f\r ]/;
const COVERED_ITEM = /^(?:[-*][\t\n\f\r ]*)?(?:AC-)?([0-9]+)[\t\n\f\r ]*$/;
const GO_MAX_INT = 9223372036854775807n;

/** Line indexes of `headings` found in order, each searched after the one before; -1 where not found. */
export function headingPositions(lines: readonly string[], headings: readonly string[]): number[] {
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

/**
 * The [start, end) lines of the section `heading` opens, its own line
 * included, up to the next of `headings` found after it; null when the
 * heading is not one of `headings` or is not in the text. Sections are cut at
 * the required headings only, the way the structure checks read the file.
 */
export function sectionRange(
  lines: readonly string[],
  headings: readonly string[],
  heading: string,
): { readonly start: number; readonly end: number } | null {
  const at = headings.indexOf(heading);
  if (at === -1) return null;
  const positions = headingPositions(lines, headings);
  const start = positions[at] ?? -1;
  if (start === -1) return null;
  const next = positions.slice(at + 1).find((position) => position !== -1);
  return { start, end: next ?? lines.length };
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
  readonly startLine: number;
  readonly endLine: number;
}

/**
 * Where one criterion ends and the next begins (`splitAcceptanceCriteriaBlocks`):
 * a criterion starts at an unindented "N." or "N)" line, and every following
 * non-blank line up to the next one belongs to it. Lines before the first
 * criterion are dropped.
 */
const DERIVED_CRITERION_ITEM =
  /^(?:[-*+][\t\n\f\r ]+[^\t\n\f\r ]|[0-9]+[.)][\t\n\f\r ]+[^\t\n\f\r ])/;
const DERIVED_CRITERION_MARKER = /^(?:[-*+][\t\n\f\r ]+|[0-9]+[.)][\t\n\f\r ]+)([\s\S]*)$/;
const TASK_LIST_MARKER = /^\[[ xX]\](?:[\t\n\f\r ]+|$)/;
const EXACT_NUMBERED_ITEM = /^[0-9]+[.)][ \t]+/;
const EXACT_BULLETED_ITEM = /^[-*+][ \t]+/;
const NON_REQUIREMENT_SECTIONS = new Set([
  "problem",
  "non-goals",
  "out of scope",
  "risks",
  "open questions",
  "background",
  "context",
  "motivation",
  "alternatives considered",
  "references",
  "notes",
]);

/** The level of an unindented ATX heading, or zero for any other line. */
export function markdownHeadingLevel(raw: string): number {
  if (raw === "" || raw !== raw.replace(/^[ \t]/, "") || raw.charAt(0) !== "#") return 0;
  let level = 0;
  while (raw.charAt(level) === "#") level++;
  if (level === 0 || level > 6 || level >= raw.length || !/[ \t]/.test(raw.charAt(level))) return 0;
  return level;
}

/** The first line after a closed leading YAML front-matter block, or zero. */
export function frontMatterBodyOffset(lines: readonly string[]): number {
  if (lines.length === 0 || trimSpace((lines[0] ?? "").replace(/^\uFEFF/, "")) !== "---") return 0;
  let keyFound = false;
  for (let i = 1; i < lines.length; i++) {
    const marker = trimSpace(lines[i] ?? "");
    if (!keyFound) {
      if (marker === "") continue;
      if (!/^[A-Za-z0-9_-]+:/.test(marker)) return 0;
      keyFound = true;
      continue;
    }
    if (marker === "---" || marker === "...") return i + 1;
  }
  return 0;
}

function asciiLower(text: string): string {
  return text.replace(/[A-Z]/g, (char) => char.toLowerCase());
}

/** The shared heading comparison used by derivation, exclusions and decisions. */
export function normalizeSpecHeading(raw: string): string {
  let text = trimSpace(raw);
  const level = markdownHeadingLevel(text);
  if (level > 0) text = trimSpace(text.slice(level));
  return asciiLower(trimSpace(text.endsWith(":") ? text.slice(0, -1) : text));
}

/** A heading's displayed text, retaining a trailing colon when it was written. */
export function headingLabel(raw: string): string {
  const text = trimSpace(raw);
  const level = markdownHeadingLevel(text);
  return level > 0 ? trimSpace(text.slice(level)) : text;
}

function markdownLineMask(lines: readonly string[]): boolean[] {
  const blocked = lines.map(() => false);
  const bodyOffset = frontMatterBodyOffset(lines);
  for (let i = 0; i < bodyOffset && i < blocked.length; i++) blocked[i] = true;
  let fence: { readonly ch: string; readonly length: number } | null = null;
  let inHTMLComment = false;
  lines.forEach((raw, i) => {
    if (i < bodyOffset) return;
    const trimmed = trimSpace(raw);
    if (fence !== null) {
      blocked[i] = true;
      if (closesFence(trimmed, fence.ch, fence.length)) fence = null;
      return;
    }
    if (inHTMLComment) {
      blocked[i] = true;
      if (raw.includes("-->")) inHTMLComment = false;
      return;
    }
    const commentText = raw.replace(/^ +/, "");
    const indent = raw.length - commentText.length;
    if (indent <= 3 && commentText.startsWith("<!--")) {
      blocked[i] = true;
      if (!raw.includes("-->")) inHTMLComment = true;
      return;
    }
    const opened = fenceMarker(trimmed);
    if (opened !== null) {
      blocked[i] = true;
      fence = opened;
    }
  });
  return blocked;
}

function headingIsOutsideFence(mask: readonly boolean[], wanted: number): boolean {
  return !(mask[wanted] ?? false);
}

function sectionEnd(
  lines: readonly string[],
  headingLine: number,
  level: number,
  mask: readonly boolean[],
): number {
  for (let i = headingLine + 1; i < lines.length; i++) {
    const raw = lines[i] ?? "";
    if (mask[i]) continue;
    let got = markdownHeadingLevel(raw);
    if (got === 0 && level === 2 && trimSpace(raw).startsWith("## ")) got = 2;
    if (got > 0 && got <= level) return i;
  }
  return lines.length;
}

function exactCriteriaSection(
  lines: readonly string[],
  mask = markdownLineMask(lines),
): { start: number; end: number } | null {
  for (let i = 0; i < lines.length; i++) {
    if (trimSpace(lines[i] ?? "") !== ACCEPTANCE_CRITERIA || !headingIsOutsideFence(mask, i))
      continue;
    return { start: i + 1, end: sectionEnd(lines, i, 2, mask) };
  }
  return null;
}

function namedCriteriaSection(
  lines: readonly string[],
  mask = markdownLineMask(lines),
): { start: number; end: number; heading: string } | null {
  for (let i = 0; i < lines.length; i++) {
    const raw = lines[i] ?? "";
    const level = markdownHeadingLevel(raw);
    if (
      level < 2 ||
      level > 6 ||
      !headingIsOutsideFence(mask, i) ||
      trimSpace(raw) === ACCEPTANCE_CRITERIA ||
      normalizeSpecHeading(raw) !== "acceptance criteria"
    ) {
      continue;
    }
    return { start: i + 1, end: sectionEnd(lines, i, level, mask), heading: trimSpace(raw) };
  }
  return null;
}

function isThematicBreak(line: string): boolean {
  const compact = line.replace(/[ \t]/g, "");
  if (compact.length < 3 || ![45, 42, 95].includes(compact.charCodeAt(0))) return false;
  const marker = compact.charAt(0);
  for (let i = 1; i < compact.length; i++) {
    if (compact.charAt(i) !== marker) return false;
  }
  return true;
}

function splitCriteriaBlocks(
  sectionLines: readonly string[],
  itemPattern = CRITERION_ITEM,
  stopAtHeading = false,
  mask: readonly boolean[] = markdownLineMask(sectionLines),
): CriterionBlock[] {
  const blocks: CriterionBlock[] = [];
  let current: string[] | null = null;
  let currentStart = -1;
  let currentEnd = -1;
  const flush = () => {
    if (current !== null && current.length > 0) {
      blocks.push({
        rawLines: current,
        folded: current.map(trimSpace).join(" "),
        startLine: currentStart,
        endLine: currentEnd,
      });
    }
    current = null;
    currentStart = -1;
    currentEnd = -1;
  };
  let blankAfterItem = false;
  sectionLines.forEach((raw, lineIndex) => {
    if (mask[lineIndex] ?? false) {
      if (current !== null && trimSpace(raw) !== "") {
        current.push(raw);
        currentEnd = lineIndex;
        blankAfterItem = false;
      }
      return;
    }
    const trimmed = trimSpace(raw);
    if (trimmed === "") {
      if (stopAtHeading && current !== null) blankAfterItem = true;
      return;
    }
    if (stopAtHeading && isThematicBreak(trimmed)) {
      flush();
      blankAfterItem = false;
      return;
    }
    const indented = isIndented(raw);
    if (stopAtHeading && !indented && markdownHeadingLevel(raw) > 0) {
      flush();
      blankAfterItem = false;
      return;
    }
    if (!indented && itemPattern.test(trimmed)) {
      flush();
      current = [];
      currentStart = lineIndex;
      currentEnd = lineIndex;
      blankAfterItem = false;
    } else if (stopAtHeading && current !== null && blankAfterItem && !indented) {
      flush();
      blankAfterItem = false;
    }
    if (current !== null) {
      current.push(raw);
      currentEnd = lineIndex;
      blankAfterItem = false;
    }
  });
  flush();
  return blocks;
}

/** The [start, end) lines under the first "## Acceptance criteria", up to the next "## " line; null without the heading. */
export function criteriaSectionRange(
  lines: readonly string[],
): { readonly start: number; readonly end: number } | null {
  return exactCriteriaSection(lines);
}

interface ParsedCriteria {
  readonly criteria: string[];
  readonly locations: SpecCriterionLocation[];
  readonly sections: string[];
  readonly named: boolean;
  readonly heading: string;
}

export interface SpecCriterionLocation {
  readonly text: string;
  readonly section: string;
  readonly item: number;
  readonly startLine: number;
  readonly endLine: number;
}

function deriveSectionCriteria(
  lines: readonly string[],
  offset: number,
  source: string,
  named: boolean,
  mask: readonly boolean[] = markdownLineMask(lines),
): ParsedCriteria {
  const criteria: string[] = [];
  const locations: SpecCriterionLocation[] = [];
  splitCriteriaBlocks(lines, DERIVED_CRITERION_ITEM, true, mask).forEach((block) => {
    const match = DERIVED_CRITERION_MARKER.exec(trimSpace(block.folded));
    if (match === null) return;
    const text = trimSpace((match[1] ?? "").replace(TASK_LIST_MARKER, ""));
    if (text === "") return;
    const item = criteria.length + 1;
    const criterion = `${item}. ${text}`;
    criteria.push(criterion);
    locations.push({
      text: criterion,
      section: source,
      item,
      startLine: offset + block.startLine,
      endLine: offset + block.endLine,
    });
  });
  return { criteria, locations, sections: [], named, heading: source };
}

function deriveSourceName(lines: readonly string[]): string {
  const heading = lines.find((line) => markdownHeadingLevel(line) === 1);
  return heading === undefined ? "document title" : headingLabel(heading);
}

function deriveSourceSection(lines: readonly string[]): string {
  const heading = lines.find((line) => markdownHeadingLevel(line) === 1);
  return heading === undefined ? "" : trimSpace(heading);
}

function parseDerivedCriteria(lines: readonly string[]): ParsedCriteria {
  const mask = markdownLineMask(lines);
  const headingLines = lines.flatMap((raw, index) =>
    [1, 2].includes(markdownHeadingLevel(raw)) && headingIsOutsideFence(mask, index) ? [index] : [],
  );
  const criteria: string[] = [];
  const locations: SpecCriterionLocation[] = [];
  const sections: string[] = [];
  const sectionsSeen = new Set<string>();
  const firstHeading = headingLines[0] ?? lines.length;
  const titleArea = lines.slice(0, firstHeading);
  const titleAreaSection = deriveSourceSection(titleArea);
  const titleAreaName = deriveSourceName(titleArea);
  for (let i = 0; i <= headingLines.length; i++) {
    const start = i === 0 ? 0 : (headingLines[i - 1] ?? 0) + 1;
    const end = i < headingLines.length ? (headingLines[i] ?? lines.length) : lines.length;
    const heading = i === 0 ? null : (lines[headingLines[i - 1] ?? 0] ?? "");
    const active = heading === null || !NON_REQUIREMENT_SECTIONS.has(normalizeSpecHeading(heading));
    if (!active || start >= end) continue;
    const section = heading === null ? titleAreaSection : trimSpace(heading);
    const source = deriveSectionCriteria(
      lines.slice(start, end),
      start,
      section,
      false,
      mask.slice(start, end),
    );
    if (source.criteria.length === 0) continue;
    const name = heading === null ? titleAreaName : headingLabel(heading);
    if (!sectionsSeen.has(name)) {
      sectionsSeen.add(name);
      sections.push(name);
    }
    source.criteria.forEach((criterion, criterionIndex) => {
      const body = trimSpace(criterion.replace(/^[0-9]+[.)][\t\n\f\r ]*/, ""));
      const text = `${criteria.length + 1}. ${body}`;
      const location = source.locations[criterionIndex];
      if (location === undefined) return;
      criteria.push(text);
      locations.push({ ...location, text, item: criteria.length });
    });
  }
  return { criteria, locations, sections, named: false, heading: "" };
}

function exactCriteriaKinds(
  lines: readonly string[],
  mask: readonly boolean[],
  offset: number,
): { readonly numbered: boolean; readonly bulleted: boolean; readonly lines: number[] } {
  let numbered = false;
  let bulleted = false;
  const itemLines: number[] = [];
  lines.forEach((raw, index) => {
    if ((mask[index] ?? false) || isIndented(raw)) return;
    if (EXACT_NUMBERED_ITEM.test(raw)) {
      numbered = true;
      itemLines.push(offset + index + 1);
    } else if (EXACT_BULLETED_ITEM.test(raw)) {
      bulleted = true;
      itemLines.push(offset + index + 1);
    }
  });
  return { numbered, bulleted, lines: itemLines };
}

function parseCriteria(content: string): ParsedCriteria {
  const lines = content.split("\n");
  const mask = markdownLineMask(lines);
  const exact = exactCriteriaSection(lines, mask);
  if (exact !== null) {
    const sectionLines = lines.slice(exact.start, exact.end);
    const sectionMask = mask.slice(exact.start, exact.end);
    const kinds = exactCriteriaKinds(sectionLines, sectionMask, exact.start);
    if (!kinds.numbered) {
      const derived = deriveSectionCriteria(
        sectionLines,
        exact.start,
        ACCEPTANCE_CRITERIA,
        true,
        sectionMask,
      );
      return {
        ...derived,
        named: false,
        sections: derived.criteria.length > 0 ? [ACCEPTANCE_CRITERIA] : [],
      };
    }
    const blocks = splitCriteriaBlocks(sectionLines, CRITERION_ITEM, false, sectionMask);
    return {
      criteria: blocks.map((block) => block.folded),
      locations: blocks.map((block, i) => ({
        text: block.folded,
        section: ACCEPTANCE_CRITERIA,
        item: i + 1,
        startLine: exact.start + block.startLine,
        endLine: exact.start + block.endLine,
      })),
      sections: [],
      named: false,
      heading: "",
    };
  }
  const named = namedCriteriaSection(lines, mask);
  if (named !== null)
    return deriveSectionCriteria(
      lines.slice(named.start, named.end),
      named.start,
      named.heading,
      true,
      mask.slice(named.start, named.end),
    );
  return parseDerivedCriteria(lines);
}

/** The spec's criteria as the server reads them, including derived operator-shaped criteria. */
export function specAcceptanceCriteria(content: string): string[] {
  return parseCriteria(content).criteria;
}

/** The structural source locations of the criteria the server numbers. */
export function specCriterionLocations(content: string): SpecCriterionLocation[] {
  return parseCriteria(content).locations;
}

/** All unindented ATX headings outside fences, in document order. */
export function specDocumentHeadings(content: string): string[] {
  const lines = content.split("\n");
  const mask = markdownLineMask(lines);
  return lines.flatMap((raw, index) => {
    const trimmed = trimSpace(raw);
    return markdownHeadingLevel(raw) > 0 && headingIsOutsideFence(mask, index) ? [trimmed] : [];
  });
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
  /** The first heading with nothing but blank lines under it ("## Plan" is a container and "## Out of scope" is human prose; both exempt). */
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
    (heading, i) =>
      heading !== PLAN_CONTAINER && heading !== OUT_OF_SCOPE && !anyNonBlank(section(i)),
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
/** Calls `visit` with each unindented line outside a fenced code block, and its index. */
export function forEachTopLevelLine(
  lines: readonly string[],
  visit: (trimmed: string, index: number) => void,
): void {
  let fence: { readonly ch: string; readonly length: number } | null = null;
  lines.forEach((line, index) => {
    const trimmed = trimSpace(line);
    if (fence !== null) {
      if (closesFence(trimmed, fence.ch, fence.length)) fence = null;
      return;
    }
    fence = fenceMarker(trimmed);
    if (fence === null && !isIndented(line)) visit(trimmed, index);
  });
}

export function missingTicketHeaderKeys(content: string): string[] {
  const found = new Set<string>();
  forEachTopLevelLine(content.split("\n"), (trimmed) => {
    for (const key of requiredTicketHeaderKeys) if (trimmed.startsWith(key)) found.add(key);
  });
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

export function specStructure(content: string, handedOver = false): StructureCheck {
  const bytes = new TextEncoder().encode(content).byteLength;
  const sizeRow: StructureRow = {
    label: handedOver ? "Handed-over spec size" : "Spec size",
    ok: bytes <= MAX_HANDED_OVER_SPEC_BYTES,
    note:
      bytes <= MAX_HANDED_OVER_SPEC_BYTES
        ? `${bytes} bytes`
        : `${bytes} bytes, max ${MAX_HANDED_OVER_SPEC_BYTES}`,
  };
  const criteria = specAcceptanceCriteria(content);
  if (handedOver) {
    const rows: StructureRow[] = [
      sizeRow,
      {
        label: "Handed-over criteria",
        ok: criteria.length > 0,
        note: criteria.length > 0 ? `${criteria.length} parsed` : 'none parsed (want "- item")',
      },
    ];
    return {
      rows,
      passes: rows.every((row) => row.ok),
    };
  }
  const lines = content.split("\n");
  const rows: StructureRow[] = [
    sizeRow,
    ...headingRows(lines, requiredSpecHeadings, (heading, body) =>
      heading === ACCEPTANCE_CRITERIA && !anyNonBlank(body) ? "no content" : "",
    ),
  ];
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
      heading !== PLAN_CONTAINER && heading !== OUT_OF_SCOPE && !anyNonBlank(body)
        ? "no content"
        : "",
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
