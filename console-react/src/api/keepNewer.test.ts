import { keepNewer } from "@/api/keepNewer";

describe("keepNewer", () => {
  test("hands the cached and fetched values to the merge and returns its answer", () => {
    const share = keepNewer<number>((cached, fetched) => Math.max(cached ?? 0, fetched));
    expect(share(undefined, 3)).toBe(3);
    expect(share(5, 3)).toBe(5);
    expect(share(2, 3)).toBe(3);
  });
});
