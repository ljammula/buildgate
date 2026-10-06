import { ticketOraclePrefix } from "@/shared/oracle/ticketOraclePrefix";

test("a ticket's spec path maps to the directory plan approval pins its oracle under", () => {
  expect(ticketOraclePrefix("/srv/data/requests/req-1/tickets/001.spec.md")).toBe(
    "tickets/001.oracle",
  );
  expect(ticketOraclePrefix("C:\\data\\tickets\\012.spec.md")).toBe("tickets/012.oracle");
  expect(ticketOraclePrefix("002.spec.md")).toBe("tickets/002.oracle");
  expect(ticketOraclePrefix("tickets/003.md")).toBe("tickets/003.md.oracle");
});
