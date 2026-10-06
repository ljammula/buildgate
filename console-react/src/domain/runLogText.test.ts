import { readableBuildLog } from "@/domain/runLogText";

describe("readableBuildLog", () => {
  test("renders FACTORY_PROGRESS lines as steps, like factoryd watch", () => {
    const raw =
      "plain output\n" +
      'FACTORY_PROGRESS {"stage": "round", "event": "start", "round": 1, "max_rounds": 3}\n' +
      'FACTORY_PROGRESS {"stage": "agent", "event": "note", "round": 1, "detail": "bash: go test\\n ./..."}\n' +
      'FACTORY_PROGRESS {"stage": "round", "event": "end", "round": 1, "outcome": "fail", "detail": "verify failed: go test ./..."}';
    expect(readableBuildLog(raw).split("\n")).toEqual([
      "plain output",
      "▸ round 1/3    started",
      "▸ agent        bash: go test ./...",
      "▸ round 1      verify failed: go test ./...",
    ]);
  });

  test("keeps a line cut mid-stream, or one that is not a step, verbatim", () => {
    const cut = 'FACTORY_PROGRESS {"stage": "agent", "ev';
    expect(readableBuildLog(cut)).toBe(cut);
    const notAnObject = "FACTORY_PROGRESS [1, 2]";
    expect(readableBuildLog(notAnObject)).toBe(notAnObject);
  });
});
