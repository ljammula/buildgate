import { listKeys } from "./listKeys";

describe("listKeys", () => {
  it("uses the data's own key, numbering only a repeat", () => {
    expect(listKeys(["a", "b", "a", "a"], (x) => x)).toEqual(["a", "b", "a#2", "a#3"]);
  });

  it("keeps a row's key when a row is inserted before it", () => {
    const before = listKeys(["verify"], (x) => x);
    const after = listKeys(["lint", "verify"], (x) => x);
    expect(after[1]).toBe(before[0]);
  });
});
