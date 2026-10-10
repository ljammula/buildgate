import { readFileSync } from "node:fs";
import { resolve } from "node:path";

import { render } from "@testing-library/react";

import { BrandMark, brandMarkUrl } from "@/ui/BrandMark";

test("draws the small cut of the mark as decoration, its check following the text colour", () => {
  const { container } = render(<BrandMark className="size-6" />);
  const svg = container.querySelector("svg")!;
  expect(svg).toHaveAttribute("aria-hidden", "true");
  expect(svg).toHaveClass("size-6", "text-fg");
  expect(svg.querySelectorAll("rect")).toHaveLength(4);
  expect(svg.querySelector("g")).toHaveClass("fill-brand");
  expect(svg.querySelector("path")).toHaveAttribute("stroke", "currentColor");
});

test("the console's mark is the brand source, byte for byte", () => {
  // scripts/render-icons.sh copies it; an edit to either alone is drift
  // between the console and the notification icon rendered from the source.
  const served = readFileSync(resolve(process.cwd(), "public/buildgate.svg"));
  const source = readFileSync(resolve(process.cwd(), "../assets/brand/buildgate.svg"));
  expect(served.equals(source)).toBe(true);
});

test("the mark is well-formed XML, so a browser can load it as an image", () => {
  // An HTML parser tolerates what an XML parser refuses: the mark once had a
  // double hyphen inside a comment and rendered as a broken image.
  const text = readFileSync(resolve(process.cwd(), "public/buildgate.svg"), "utf8");
  const parsed = new DOMParser().parseFromString(text, "image/svg+xml");
  expect(parsed.querySelector("parsererror")).toBeNull();
  expect(parsed.documentElement.nodeName).toBe("svg");
  for (const comment of text.match(/<!--[\s\S]*?-->/g) ?? []) {
    expect(comment.slice(4, -3)).not.toContain("--");
  }
});

test("the page's tab icon is the same mark", () => {
  const html = readFileSync(resolve(process.cwd(), "index.html"), "utf8");
  expect(html).toContain(`<link rel="icon" type="image/svg+xml" href="${brandMarkUrl}" />`);
  expect(html).toContain('href="/favicon.png" data-tab-icon');
});
