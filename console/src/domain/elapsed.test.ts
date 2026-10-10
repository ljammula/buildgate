import {
  elapsedBetween,
  formatDuration,
  formatAgeCompact,
  formatElapsedCompact,
  formatAgeSeconds,
  formatLocalTimestamp,
  formatWhen,
  relativeAge,
  stallChipDisplay,
  stallStatus,
  tryParseTimestamp,
  utcTooltip,
  compareTimestamps,
} from "@/domain/elapsed";

// formatLocalTimestamp renders in the process time zone; pin it so the
// expected strings can be written out.
beforeAll(() => {
  process.env.TZ = "America/New_York";
});

describe("stallStatus", () => {
  test("returns stalled when the server reports Run.stalled", () => {
    expect(stallStatus({ stalled: true })).toBe("stalled");
  });

  test("returns null when the server reports not stalled", () => {
    expect(stallStatus({ stalled: false })).toBeNull();
  });

  test(
    "a terminal run is never stalled, no matter what the server reports " +
      "(the server itself never sets it true for a terminal run)",
    () => {
      expect(stallStatus({ stalled: false })).toBeNull();
    },
  );
});

describe("stallChipDisplay", () => {
  test("stalled chip when stalled", () => {
    expect(stallChipDisplay({ stalled: true, waitingReason: null })).toEqual({
      kind: "stalled",
      label: "stalled",
      tone: "danger",
      icon: "triangle_alert",
    });
  });

  test("waiting chip when a waiting reason is set and not stalled", () => {
    expect(
      stallChipDisplay({ stalled: false, waitingReason: "behind 1 run(s) on foo/bar" }),
    ).toEqual({
      kind: "waiting",
      label: "waiting: behind 1 run(s) on foo/bar",
      tone: "warning",
      icon: "hourglass",
    });
  });

  test("nothing when neither applies, and stalled beats a stale waiting reason", () => {
    expect(stallChipDisplay({ stalled: false, waitingReason: null })).toBeNull();
    expect(stallChipDisplay({ stalled: false, waitingReason: "" })).toBeNull();
    expect(stallChipDisplay({ stalled: true, waitingReason: "behind 1 run(s)" })?.kind).toBe(
      "stalled",
    );
  });
});

describe("formatDuration", () => {
  test("mm:ss under an hour, hh:mm:ss from an hour, zero when negative", () => {
    expect(formatDuration(0)).toBe("00:00");
    expect(formatDuration(65_000)).toBe("01:05");
    expect(formatDuration(3_599_999)).toBe("59:59");
    expect(formatDuration(3_600_000)).toBe("01:00:00");
    expect(formatDuration(3_725_000)).toBe("01:02:05");
    expect(formatDuration(-5000)).toBe("00:00");
  });
});

describe("elapsedBetween", () => {
  const now = new Date("2026-09-18T11:10:00Z");

  test("uses now for a null end, and zero for an unparseable start", () => {
    expect(elapsedBetween("2026-09-18T11:00:00Z", null, now)).toBe(600_000);
    expect(elapsedBetween("2026-09-18T11:00:00Z", "2026-09-18T11:01:00Z", now)).toBe(60_000);
    expect(elapsedBetween("", null, now)).toBe(0);
    expect(elapsedBetween("not-a-date", null, now)).toBe(0);
    expect(tryParseTimestamp("not-a-date")).toBeNull();
  });
});

describe("formatLocalTimestamp", () => {
  test("renders an RFC3339 UTC timestamp in local time", () => {
    expect(formatLocalTimestamp("2026-09-18T11:00:00Z")).toBe("2026-09-18 07:00:00");
  });

  test("falls back to the raw value when unparseable", () => {
    expect(formatLocalTimestamp("not-a-date")).toBe("not-a-date");
  });

  test("falls back to the raw (empty) value when empty", () => {
    expect(formatLocalTimestamp("")).toBe("");
  });
});

describe("utcTooltip", () => {
  test("shows the UTC value, or null when unparseable", () => {
    expect(utcTooltip("2026-09-18T11:00:00Z")).toBe("UTC: 2026-09-18T11:00:00.000Z");
    expect(utcTooltip("not-a-date")).toBeNull();
  });
});

describe("formatElapsedCompact", () => {
  const minute = 60_000;
  test.each([
    [0, "00:00"],
    [20 * minute, "20:00"],
    [59 * minute + 59_000, "59:59"],
    [60 * minute, "1h 00m"],
    [125 * minute, "2h 05m"],
    [23 * 60 * minute + 59 * minute, "23h 59m"],
    [24 * 60 * minute, "1d 0h"],
    [(3 * 24 + 4) * 60 * minute + 30 * minute, "3d 4h"],
    [-5000, "00:00"],
  ])("%d ms reads %s", (ms, want) => {
    expect(formatElapsedCompact(ms)).toBe(want);
  });
});

describe("relativeAge", () => {
  const now = new Date("2026-10-05T12:00:00Z");
  test.each([
    ["2026-10-05T11:59:30Z", "just now"],
    ["2026-10-05T11:55:00Z", "5m ago"],
    ["2026-10-05T09:55:00Z", "2h 05m ago"],
    ["2026-10-02T08:00:00Z", "3d 4h ago"],
    ["2026-10-05T12:00:30Z", "just now"],
  ])("%s reads %s", (at, want) => {
    expect(relativeAge(at, now)).toBe(want);
  });

  test("an empty or unparseable timestamp comes back verbatim", () => {
    expect(relativeAge("", now)).toBe("");
    expect(relativeAge("soon", now)).toBe("soon");
  });
});

describe("formatAgeCompact", () => {
  const minute = 60_000;
  test.each([
    [0, "0m"],
    [45 * minute, "45m"],
    [60 * minute, "1h"],
    [125 * minute, "2h 05m"],
    [24 * 60 * minute, "1d"],
    [(3 * 24 + 4) * 60 * minute, "3d 4h"],
  ])("%d ms reads %s", (ms, want) => {
    expect(formatAgeCompact(ms)).toBe(want);
  });
});

describe("compareTimestamps", () => {
  test("a dropped fraction does not sort after a written one", () => {
    // As text, "…05Z" > "…05.5Z".
    expect(compareTimestamps("2026-10-06T10:00:05Z", "2026-10-06T10:00:05.5Z")).toBeLessThan(0);
    expect(compareTimestamps("2026-10-06T10:00:05.5Z", "2026-10-06T10:00:05Z")).toBeGreaterThan(0);
  });

  test("fractions of different lengths compare by value", () => {
    expect(
      compareTimestamps("2026-10-06T04:13:53.20754Z", "2026-10-06T04:13:53.819097Z"),
    ).toBeLessThan(0);
    expect(
      compareTimestamps("2026-10-06T04:13:53.9Z", "2026-10-06T04:13:53.819097Z"),
    ).toBeGreaterThan(0);
  });

  test("instants in the same millisecond compare by their fraction", () => {
    expect(
      compareTimestamps("2026-10-06T04:13:53.000469Z", "2026-10-06T04:13:53.000470Z"),
    ).toBeLessThan(0);
  });

  test("an offset is an instant, not text", () => {
    expect(compareTimestamps("2026-10-05T22:50:20-05:00", "2026-10-06T03:50:21Z")).toBeLessThan(0);
    expect(compareTimestamps("2026-10-05T22:50:20-05:00", "2026-10-06T03:50:20Z")).toBe(0);
  });

  test("an unreadable timestamp orders as equal", () => {
    expect(compareTimestamps("", "2026-10-06T03:50:20Z")).toBe(0);
  });
});

describe("formatAgeSeconds", () => {
  test("whole seconds, floored, with no minutes form", () => {
    expect(formatAgeSeconds(0)).toBe("0s");
    expect(formatAgeSeconds(999)).toBe("0s");
    expect(formatAgeSeconds(5_400)).toBe("5s");
    expect(formatAgeSeconds(75_000)).toBe("75s");
  });

  test("a negative age (clock skew) is 0s", () => {
    expect(formatAgeSeconds(-3_000)).toBe("0s");
  });
});

describe("formatWhen", () => {
  test("HH:mm on the same local day, 'MMM d HH:mm' otherwise, the raw text when unparseable", () => {
    const now = new Date(2026, 8, 15, 12, 0, 0);
    expect(formatWhen(new Date(2026, 8, 15, 9, 5).toISOString(), now)).toBe("09:05");
    expect(formatWhen(new Date(2026, 8, 14, 23, 59).toISOString(), now)).toBe("Sep 14 23:59");
    expect(formatWhen("not a time", now)).toBe("not a time");
    expect(formatWhen("", now)).toBe("");
  });
});
