import { badgeLabel, setFaviconBadge } from "@/platform/faviconBadge";

function addIconLink(): HTMLLinkElement {
  const link = document.createElement("link");
  link.rel = "alternate icon";
  link.href = "/favicon.png";
  link.setAttribute("data-tab-icon", "");
  document.head.append(link);
  return link;
}

afterEach(() => {
  document.head.querySelectorAll("link").forEach((link) => {
    link.remove();
  });
  vi.unstubAllGlobals();
});

test("the label is the count, capped at 99+", () => {
  expect(badgeLabel(1)).toBe("1");
  expect(badgeLabel(99)).toBe("99");
  expect(badgeLabel(100)).toBe("99+");
});

test("a count draws onto the icon and makes the drawn PNG the page's icon", () => {
  const link = addIconLink();
  const drawn: string[] = [];
  const context = {
    drawImage: () => drawn.push("icon"),
    beginPath: () => undefined,
    arc: () => drawn.push("disc"),
    fill: () => undefined,
    fillText: (text: string) => drawn.push(`text:${text}`),
  };
  vi.spyOn(HTMLCanvasElement.prototype, "getContext").mockReturnValue(
    context as unknown as CanvasRenderingContext2D,
  );
  vi.spyOn(HTMLCanvasElement.prototype, "toDataURL").mockReturnValue("data:image/png;base64,AAAA");
  // The icon loads at once.
  vi.stubGlobal(
    "Image",
    class {
      onload: (() => void) | null = null;
      set src(_value: string) {
        this.onload?.();
      }
    },
  );
  setFaviconBadge(120);
  expect(drawn).toEqual(["icon", "disc", "text:99+"]);
  expect(link.rel).toBe("icon");
  expect(link.getAttribute("href")).toBe("data:image/png;base64,AAAA");
});

test("zero restores the plain icon", () => {
  const link = addIconLink();
  link.rel = "icon";
  link.href = "data:image/png;base64,AAAA";
  setFaviconBadge(0);
  expect(link.rel).toBe("alternate icon");
  expect(link.getAttribute("href")).toBe("/favicon.png");
});

test("a page with no icon link, or no canvas, is left alone", () => {
  expect(() => {
    setFaviconBadge(3);
  }).not.toThrow();
});
