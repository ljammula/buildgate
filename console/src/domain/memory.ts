import {
  type JsonObject,
  numberOr,
  objectList,
  optString,
  reqBoolean,
  reqNumber,
  reqString,
  stringList,
} from "@/domain/decode";

/**
 * One candidate line of repository memory: internal/api.MemoryCandidate. The
 * line is a build agent's or the operator's note, so it is shown as text.
 */
export interface MemoryCandidate {
  readonly id: string;
  readonly line: string;
  /** "agent" or "operator". */
  readonly source: string;
  /** "candidate", "proposed" or "dropped". */
  readonly state: string;
  /** How many runs said the line. */
  readonly seen: number;
  readonly firstSeenAt: string;
  readonly lastSeenAt: string;
  /** The memory request that proposes the line; "" when none does. */
  readonly requestId: string;
}

/** internal/api.ProjectMemory: GET /projects/{project}/memory. */
export interface ProjectMemory {
  readonly project: string;
  readonly on: boolean;
  /** Which switch has memory off; "" when it is on. */
  readonly offReason: string;
  readonly budgetLines: number;
  readonly budgetChars: number;
  readonly usedLines: number;
  readonly usedChars: number;
  /** The fenced section of AGENTS.md at the checkout's HEAD, in order. */
  readonly inForce: readonly string[];
  /** Why the section could not be read; "" when it was. */
  readonly sectionError: string;
  readonly candidates: readonly MemoryCandidate[];
}

function decodeCandidate(o: JsonObject, at: string): MemoryCandidate {
  return {
    id: reqString(o, "id", at),
    line: reqString(o, "line", at),
    source: optString(o, "source", at),
    state: reqString(o, "state", at),
    seen: numberOr(o, "seen", at, 0),
    firstSeenAt: optString(o, "first_seen_at", at),
    lastSeenAt: optString(o, "last_seen_at", at),
    requestId: optString(o, "request_id", at),
  };
}

export function decodeProjectMemory(o: JsonObject, at: string): ProjectMemory {
  return {
    project: reqString(o, "project", at),
    on: reqBoolean(o, "on", at),
    offReason: optString(o, "off_reason", at),
    budgetLines: reqNumber(o, "budget_lines", at),
    budgetChars: reqNumber(o, "budget_chars", at),
    usedLines: reqNumber(o, "used_lines", at),
    usedChars: reqNumber(o, "used_chars", at),
    inForce: stringList(o, "in_force", at),
    sectionError: optString(o, "section_error", at),
    candidates: objectList(o, "candidates", at, decodeCandidate),
  };
}

/** "3 of 40 lines, 120 of 3000 characters". */
export function memoryBudgetText(memory: ProjectMemory): string {
  return `${memory.usedLines} of ${memory.budgetLines} lines, ${memory.usedChars} of ${memory.budgetChars} characters`;
}
