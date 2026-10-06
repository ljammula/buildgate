/**
 * The live build log as shown: a worker's `FACTORY_PROGRESS <json>`
 * protocol lines become one readable step each, the way `factoryd watch`
 * renders them ("round 1/3  started", "agent  read: main.go"); every other
 * line, and any line that does not parse (a chunk cut mid-line), is kept
 * verbatim.
 */
export function readableBuildLog(raw: string): string {
  const prefix = "FACTORY_PROGRESS ";
  return raw
    .split("\n")
    .map((line) => {
      if (!line.startsWith(prefix)) return line;
      let event: unknown;
      try {
        event = JSON.parse(line.slice(prefix.length));
      } catch {
        return line;
      }
      if (typeof event !== "object" || event === null || Array.isArray(event)) return line;
      return progressStep(event as Record<string, unknown>) ?? line;
    })
    .join("\n");
}

function intOr0(value: unknown): number {
  return typeof value === "number" && Number.isInteger(value) ? value : 0;
}

/** One readable step for a progress object, or null when it is not one (no string stage and event). */
export function progressStep(e: Record<string, unknown>): string | null {
  const stage = e.stage;
  const kind = e.event;
  if (typeof stage !== "string" || typeof kind !== "string") return null;
  const round = intOr0(e.round);
  const maxRounds = intOr0(e.max_rounds);
  const detail = (typeof e.detail === "string" ? e.detail : "").replace(/\s+/g, " ").trim();
  const outcome = typeof e.outcome === "string" ? e.outcome : "";
  const label =
    stage === "round" && round > 0
      ? `round ${round}${maxRounds > 0 ? `/${maxRounds}` : ""}`
      : stage;
  let text: string;
  switch (kind) {
    case "start":
      text = "started";
      break;
    case "end":
      text =
        stage === "round" && outcome !== "" && outcome !== "pass" && detail !== ""
          ? detail
          : outcome === ""
            ? "done"
            : outcome;
      break;
    default:
      text = detail === "" ? kind : detail;
  }
  return `▸ ${label.padEnd(12)} ${text}`;
}
