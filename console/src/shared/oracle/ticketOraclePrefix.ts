/**
 * "…/tickets/001.spec.md" -> "tickets/001.oracle": the key plan approval pins
 * a ticket's oracle files under (internal/request.TicketOracleDir).
 */
export function ticketOraclePrefix(specPath: string): string {
  const parts = specPath.replaceAll("\\", "/").split("/");
  const base = parts[parts.length - 1] ?? "";
  return `tickets/${base.replace(/\.spec\.md$/, "")}.oracle`;
}
