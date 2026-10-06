import { openInNewTab } from "@/platform/openInNewTab";

test("opens a URL in a new tab", () => {
  const open = vi.spyOn(window, "open").mockImplementation(() => null);
  openInNewTab("https://temporal.example/runs/7");
  expect(open).toHaveBeenCalledWith("https://temporal.example/runs/7", "_blank");
  open.mockRestore();
});

test("does not throw when the browser refuses to open a tab", () => {
  const open = vi.spyOn(window, "open").mockImplementation(() => {
    throw new Error("blocked");
  });
  expect(() => {
    openInNewTab("https://example.test");
  }).not.toThrow();
  open.mockRestore();
});
