import { render, screen } from "@testing-library/react";

import { decodeRequestSummary } from "@/domain/request";
import { TicketRollupStrip } from "@/ui/TicketRollupStrip";

function requestWith(tickets: Record<string, unknown>[], state = "building") {
  return decodeRequestSummary(
    {
      id: "req-1",
      workspace: "ws",
      project: "proj",
      state,
      submitted_at: "",
      updated_at: "",
      ticket_index: 2,
      ticket_count: tickets.length,
      tickets,
    },
    "test",
  );
}

test("renders nothing for a request with no tickets yet", () => {
  const { container } = render(<TicketRollupStrip request={requestWith([])} />);
  expect(container).toBeEmptyDOMElement();
});

test("shows a count and label per bucket, most actionable first, each with its tone", () => {
  render(
    <TicketRollupStrip
      request={requestWith([{ index: 1, run_id: "r1" }, { index: 2, run_id: "r2" }, { index: 3 }])}
    />,
  );
  const items = screen.getAllByRole("listitem").map((li) => li.textContent);
  expect(items).toEqual(["1 building", "1 queued", "1 done"]);
  const dot = screen.getAllByRole("listitem")[0]?.querySelector("[data-tone]");
  expect(dot).toHaveAttribute("data-tone", "info");
});
