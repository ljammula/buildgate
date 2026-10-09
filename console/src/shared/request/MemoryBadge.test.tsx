import { render, screen } from "@testing-library/react";

import { decodeRequestSummary } from "@/domain/request";

import { MemoryBadge } from "./MemoryBadge";

const route = "GET /requests/{id}";

function request(source: unknown) {
  return decodeRequestSummary(
    {
      id: "req-1",
      workspace: "/w",
      project: "p",
      state: "spec_review",
      submitted_at: "2026-09-28T00:00:00Z",
      updated_at: "2026-09-28T00:00:00Z",
      ...(source === undefined ? {} : { source }),
    },
    route,
  );
}

test("a memory request is decoded from source.kind and labelled memory", () => {
  const memory = request({ kind: "memory" });
  expect(memory.sourceKind).toBe("memory");
  render(<MemoryBadge request={memory} />);
  expect(screen.getByTestId("request-memory-badge")).toHaveTextContent("memory");
});

test("any other request, or one with no source, has no memory label", () => {
  expect(request({ kind: "issue", issue_ref: "acme/app#7" }).sourceKind).toBe("issue");
  expect(request(undefined).sourceKind).toBe("");
  render(
    <>
      <MemoryBadge request={request({ kind: "text" })} />
      <MemoryBadge request={request(undefined)} />
    </>,
  );
  expect(screen.queryByTestId("request-memory-badge")).not.toBeInTheDocument();
});
