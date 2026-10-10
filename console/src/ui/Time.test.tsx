import { act, render, screen } from "@testing-library/react";

import { formatLocalTimestamp } from "@/domain/elapsed";
import { ElapsedText, LocalTimeText, StallChip } from "@/ui/Time";

afterEach(() => {
  vi.useRealTimers();
});

describe("StallChip", () => {
  test("renders the stalled chip when Run.stalled is true", () => {
    render(<StallChip run={{ stalled: true, waitingReason: null }} />);
    expect(screen.getByTestId("stalled-chip")).toHaveAttribute("data-tone", "danger");
    expect(screen.getByText("Stalled")).toBeInTheDocument();
    expect(screen.queryByTestId("waiting-chip")).not.toBeInTheDocument();
  });

  test("renders the waiting chip, not stalled, when waitingReason is set and Run.stalled is false", () => {
    render(<StallChip run={{ stalled: false, waitingReason: "behind 1 run(s) on foo/bar" }} />);
    expect(screen.getByTestId("waiting-chip")).toHaveAttribute("data-tone", "warning");
    expect(screen.getByText("Waiting: behind 1 run(s) on foo/bar")).toBeInTheDocument();
    expect(screen.queryByTestId("stalled-chip")).not.toBeInTheDocument();
  });

  test("renders nothing when neither condition applies", () => {
    const { container } = render(<StallChip run={{ stalled: false, waitingReason: null }} />);
    expect(container).toBeEmptyDOMElement();
  });

  test("stalled takes priority over a stale waiting reason", () => {
    render(<StallChip run={{ stalled: true, waitingReason: "behind 1 run(s) on foo/bar" }} />);
    expect(screen.getByTestId("stalled-chip")).toBeInTheDocument();
    expect(screen.queryByTestId("waiting-chip")).not.toBeInTheDocument();
  });
});

describe("LocalTimeText", () => {
  test("shows the local time with the UTC value in a tooltip", () => {
    const value = "2026-09-18T11:00:00Z";
    render(<LocalTimeText value={value} />);
    const el = screen.getByText(formatLocalTimestamp(value));
    expect(el.tagName).toBe("TIME");
    expect(el).toHaveAttribute("dateTime", value);
    expect(el).toHaveAttribute("title", "UTC: 2026-09-18T11:00:00.000Z");
  });

  test("falls back to the raw value when unparseable", () => {
    render(<LocalTimeText value="not-a-date" />);
    const el = screen.getByText("not-a-date");
    expect(el).not.toHaveAttribute("title");
    expect(el.tagName).not.toBe("TIME");
  });
});

describe("ElapsedText", () => {
  test("ticks while open-ended and leaves no timer after unmount", () => {
    vi.useFakeTimers();
    vi.setSystemTime(new Date("2026-09-18T11:00:05Z"));
    const { unmount } = render(<ElapsedText since="2026-09-18T11:00:00Z" />);
    expect(screen.getByText("00:05")).toBeInTheDocument();
    act(() => {
      vi.advanceTimersByTime(3000);
    });
    expect(screen.getByText("00:08")).toBeInTheDocument();
    unmount();
    expect(vi.getTimerCount()).toBe(0);
  });

  test("fifty counters share one timer, and unmounting them all leaves none", () => {
    vi.useFakeTimers();
    vi.setSystemTime(new Date("2026-09-18T11:00:05Z"));
    const { unmount } = render(
      <>
        {Array.from({ length: 50 }, (_, i) => (
          <ElapsedText key={i} since="2026-09-18T11:00:00Z" />
        ))}
      </>,
    );
    expect(vi.getTimerCount()).toBe(1);
    expect(screen.getAllByText("00:05")).toHaveLength(50);
    act(() => {
      vi.advanceTimersByTime(2000);
    });
    expect(screen.getAllByText("00:07")).toHaveLength(50);
    unmount();
    expect(vi.getTimerCount()).toBe(0);
  });

  test("each distinct interval has its own timer, which stops with its last subscriber", () => {
    vi.useFakeTimers();
    const a = render(<ElapsedText since="" intervalMs={1000} />);
    const b = render(<ElapsedText since="" intervalMs={60_000} />);
    const c = render(<ElapsedText since="" intervalMs={1000} />);
    expect(vi.getTimerCount()).toBe(2);
    a.unmount();
    expect(vi.getTimerCount()).toBe(2);
    c.unmount();
    expect(vi.getTimerCount()).toBe(1);
    b.unmount();
    expect(vi.getTimerCount()).toBe(0);
  });

  test("a finished span is fixed and starts no timer", () => {
    vi.useFakeTimers();
    render(<ElapsedText since="2026-09-18T11:00:00Z" until="2026-09-18T12:01:01Z" />);
    expect(screen.getByText("1h 01m")).toBeInTheDocument();
    expect(vi.getTimerCount()).toBe(0);
  });

  test("an unparseable start renders zero", () => {
    vi.useFakeTimers();
    render(<ElapsedText since="" />);
    expect(screen.getByText("00:00")).toBeInTheDocument();
  });
});
