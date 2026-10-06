import {
  elapsedBetween,
  formatDuration,
  formatLocalTimestamp,
  stallChipDisplay,
  stallStatus,
  tryParseTimestamp,
  utcTooltip,
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
      icon: "warning_amber",
    });
  });

  test("waiting chip when a waiting reason is set and not stalled", () => {
    expect(
      stallChipDisplay({ stalled: false, waitingReason: "behind 1 run(s) on foo/bar" }),
    ).toEqual({
      kind: "waiting",
      label: "waiting: behind 1 run(s) on foo/bar",
      tone: "warning",
      icon: "hourglass_top",
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
