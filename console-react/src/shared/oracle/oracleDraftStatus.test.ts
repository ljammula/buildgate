import { oracleDraftStatusLabel } from "@/shared/oracle/oracleDraftStatus";

test("every recorded drafting status has an operator label, and an unknown one reads as itself", () => {
  expect(oracleDraftStatusLabel("drafted")).toBe("Drafted");
  expect(oracleDraftStatusLabel("none_eligible")).toBe(
    "No criterion was judged eligible for an automated test",
  );
  expect(oracleDraftStatusLabel("failed")).toBe("Drafting failed");
  expect(oracleDraftStatusLabel("over_cap")).toBe(
    "Drafting stopped at the per-ticket file/size cap",
  );
  expect(oracleDraftStatusLabel("not_implemented")).toBe(
    "Automatic drafting is not available for this request",
  );
  expect(oracleDraftStatusLabel("something_new")).toBe("something_new");
});
