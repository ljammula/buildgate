/** What the drafting job's recorded status means to an operator. */
export function oracleDraftStatusLabel(status: string): string {
  switch (status) {
    case "drafted":
      return "Drafted";
    // Neutral on purpose: the server's own detail says why (a model's
    // one-pass classification, or -- for a non-Go workspace -- no model pass
    // at all), and this prefix used to claim a model judgment even when none
    // ran (found via a console operator walkthrough, 2026-09-24).
    case "none_eligible":
      return "No criterion was judged eligible for an automated test";
    case "failed":
      return "Drafting failed";
    case "over_cap":
      return "Drafting stopped at the per-ticket file/size cap";
    case "not_implemented":
      return "Automatic drafting is not available for this request";
    default:
      return status;
  }
}
