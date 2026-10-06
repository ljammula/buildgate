import { render, screen } from "@testing-library/react";

import { RelativeTime } from "@/ui/RelativeTime";

beforeAll(() => {
  process.env.TZ = "America/New_York";
});

afterEach(() => {
  vi.useRealTimers();
});

describe("RelativeTime", () => {
  test("reads the age, with the exact local time in the title and the instant in dateTime", () => {
    vi.useFakeTimers();
    vi.setSystemTime(new Date("2026-09-18T11:05:00Z"));
    render(<RelativeTime value="2026-09-18T11:00:00Z" />);

    const el = screen.getByText("5m ago");
    expect(el.tagName).toBe("TIME");
    expect(el).toHaveAttribute("dateTime", "2026-09-18T11:00:00Z");
    expect(el).toHaveAttribute("title", "2026-09-18 07:00:00");
  });

  test("an age over a day reads days and hours", () => {
    vi.useFakeTimers();
    vi.setSystemTime(new Date("2026-09-21T15:00:00Z"));
    render(<RelativeTime value="2026-09-18T11:00:00Z" />);

    expect(screen.getByText("3d 4h ago")).toBeInTheDocument();
  });

  test("an unparseable value comes back verbatim with no title", () => {
    render(<RelativeTime value="soon" />);

    expect(screen.getByText("soon")).not.toHaveAttribute("title");
  });
});
