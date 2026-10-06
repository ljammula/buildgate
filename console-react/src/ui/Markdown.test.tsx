import { render, screen } from "@testing-library/react";

import { Markdown } from "@/ui/Markdown";

const FORBIDDEN = ["script", "iframe", "img", "object", "embed", "style", "svg", "form"];
// Leading whitespace and control characters (U+0000-U+0020) are skipped, as a browser does.
const BAD_URL = /^[\s\p{Cc}]*(javascript|data|vbscript):/iu;

/** The DOM carries no active content, whatever the source said. */
function expectInert(container: HTMLElement): void {
  for (const tag of FORBIDDEN) {
    expect(container.querySelector(tag), `<${tag}>`).toBeNull();
  }
  for (const el of container.querySelectorAll("*")) {
    for (const attr of el.attributes) {
      expect(attr.name.startsWith("on"), `attribute ${attr.name}`).toBe(false);
      if (attr.name === "href" || attr.name === "src") {
        expect(BAD_URL.test(attr.value), `${attr.name}=${attr.value}`).toBe(false);
      }
    }
  }
}

function renderMd(source: string) {
  const view = render(<Markdown source={source} />);
  expectInert(view.container);
  return view;
}

test("renders headings, paragraphs, and lists without their markdown syntax", () => {
  const { container } = renderMd("# Title\n\nA paragraph.\n\n- one\n- two\n");
  expect(screen.getByText("Title")).toBeInTheDocument();
  expect(container.textContent).not.toContain("# Title");
  expect(screen.getByText("A paragraph.")).toBeInTheDocument();
  expect(screen.getByText("one")).toBeInTheDocument();
  expect(screen.getByText("two")).toBeInTheDocument();
});

test("never renders a heading above h2, so the page keeps its one h1", () => {
  const { container } = renderMd("# a\n\n## b\n\n###### f");
  expect(container.querySelector("h1")).toBeNull();
  expect(screen.getByRole("heading", { name: "a", level: 2 })).toBeInTheDocument();
  expect(screen.getByRole("heading", { name: "b", level: 3 })).toBeInTheDocument();
  expect(screen.getByRole("heading", { name: "f", level: 6 })).toBeInTheDocument();
});

test("renders a link with its target visible; the anchor is safe", () => {
  const { container } = renderMd("[see the spec](https://example.com/x)");
  expect(screen.getByText(/see the spec/)).toBeInTheDocument();
  expect(container.textContent).toContain("https://example.com/x");
  const a = screen.getByRole("link", { name: "see the spec" });
  expect(a).toHaveAttribute("href", "https://example.com/x");
  expect(a).toHaveAttribute("target", "_blank");
  expect(a).toHaveAttribute("rel", "noopener noreferrer nofollow");
});

test("never interprets raw HTML as markup", () => {
  const { container } = renderMd("<script>window.x = 1</script>\n\nSafe paragraph.\n");
  expect(screen.getByText("Safe paragraph.")).toBeInTheDocument();
  expect(container.textContent).toContain("<script>window.x = 1</script>");
});

test("renders a fenced code block as plain monospace text", () => {
  const { container } = renderMd("```\nmake verify\n```\n");
  expect(container.querySelector("pre code")?.textContent).toBe("make verify");
});

test("renders nested lists, ordered lists, task lists, quotes, rules and tables", () => {
  const { container } = renderMd(
    "1. a\n   - nested\n2. b\n\n- [x] done\n- [ ] todo\n\n> quoted\n\n---\n\n| h1 | h2 |\n|---|---|\n| c1 | c2 |\n",
  );
  expect(container.querySelector("ol ul")).not.toBeNull();
  const boxes = container.querySelectorAll<HTMLInputElement>('input[type="checkbox"]');
  expect(boxes).toHaveLength(2);
  expect(boxes[0]?.checked).toBe(true);
  expect(boxes[1]?.checked).toBe(false);
  expect(boxes[0]?.disabled).toBe(true);
  expect(container.querySelector("blockquote")?.textContent).toBe("quoted");
  expect(container.querySelector("hr")).not.toBeNull();
  expect(screen.getByRole("columnheader", { name: "h1" })).toBeInTheDocument();
  expect(screen.getByRole("cell", { name: "c2" })).toBeInTheDocument();
});

test("renders inline emphasis, code and breaks", () => {
  const { container } = renderMd("**b** *i* ~~d~~ `c<d>` x  \ny");
  expect(container.querySelector("strong")?.textContent).toBe("b");
  expect(container.querySelector("em")?.textContent).toBe("i");
  expect(container.querySelector("del")?.textContent).toBe("d");
  expect(container.querySelector("code")?.textContent).toBe("c<d>");
  expect(container.querySelector("br")).not.toBeNull();
});

test("shows < > & and entities exactly as written, once", () => {
  const { container } = renderMd("a < b && c > d\n\n&amp; and `x<y&z`\n\n\\*lit\\*");
  expect(container.textContent).toContain("a < b && c > d");
  expect(container.textContent).toContain("&amp; and x<y&z");
  expect(container.textContent).toContain("*lit*");
  expect(container.textContent).not.toContain("&lt;");
});

test("writes invisible and bidi characters out", () => {
  const { container } = renderMd("safe‮txt​");
  expect(container.textContent).toBe("safe\\u{202E}txt\\u{200B}");
});

describe("hostile input is visible text and the DOM stays inert", () => {
  test("script tag", () => {
    const { container } = renderMd("<script>alert(1)</script>");
    expect(container.textContent).toContain("<script>alert(1)</script>");
  });

  test("img with onerror, block and inline", () => {
    const a = renderMd("<img src=x onerror=alert(1)>");
    expect(a.container.textContent).toContain("<img src=x onerror=alert(1)>");
    const b = renderMd("text <img src=x onerror=alert(1)> more");
    expect(b.container.textContent).toContain("<img src=x onerror=alert(1)>");
  });

  test.each([
    ["javascript", "[x](javascript:alert(1))"],
    ["mixed case", "[x](JaVaScRiPt:alert(1))"],
    ["leading tab", "[x](\tjavascript:alert(1))"],
    ["leading space", "[x](<  javascript:alert(1)>)"],
    ["data", "[x](data:text/html,<script>alert(1)</script>)"],
    ["vbscript", "[x](vbscript:msgbox(1))"],
    ["relative", "[x](/etc/passwd)"],
    ["mailto", "[x](mailto:a@b.example)"],
  ])("link scheme: %s", (_name, source) => {
    const { container } = renderMd(source);
    expect(container.querySelector("a")).toBeNull();
    expect(container.textContent).toContain("x");
  });

  test("an unsafe link keeps its target visible as text", () => {
    const { container } = renderMd("[x](javascript:alert(1))");
    expect(container.textContent).toContain("javascript:alert(1)");
  });

  test("image is text, never an img", () => {
    const { container } = renderMd("![x](http://attacker.example/pixel.png)");
    expect(container.textContent).toContain("![x](http://attacker.example/pixel.png)");
  });

  test("html anchor", () => {
    const { container } = renderMd('<a href="javascript:alert(1)">x</a>');
    expect(container.textContent).toContain('<a href="javascript:alert(1)">x</a>');
  });

  test("autolink", () => {
    const { container } = renderMd("<javascript:alert(1)>");
    expect(container.querySelector("a")).toBeNull();
    expect(container.textContent).toContain("javascript:alert(1)");
  });

  test("reference-style link", () => {
    const { container } = renderMd("[x][1]\n\n[1]: javascript:alert(1)");
    expect(container.querySelector("a")).toBeNull();
    expect(container.textContent).toContain("javascript:alert(1)");
  });

  test("HTML in a table cell", () => {
    const { container } = renderMd("| a |\n|---|\n| <img src=x onerror=alert(1)> |");
    expect(container.textContent).toContain("<img src=x onerror=alert(1)>");
  });

  test("HTML in a list item", () => {
    const { container } = renderMd("- <script>alert(1)</script>");
    expect(container.textContent).toContain("<script>alert(1)</script>");
  });

  test("HTML in a heading", () => {
    const { container } = renderMd("# <script>alert(1)</script>");
    expect(container.textContent).toContain("<script>alert(1)</script>");
  });

  test("HTML in a blockquote", () => {
    const { container } = renderMd('> <iframe src="https://example.com"></iframe>');
    expect(container.textContent).toContain('<iframe src="https://example.com">');
  });

  test("svg onload", () => {
    const { container } = renderMd("<svg onload=alert(1)>");
    expect(container.textContent).toContain("<svg onload=alert(1)>");
  });

  test("iframe", () => {
    const { container } = renderMd('<iframe src="https://example.com">');
    expect(container.textContent).toContain('<iframe src="https://example.com">');
  });

  test("a very deep quote renders as text, without a crash", () => {
    const { container } = renderMd(`${">".repeat(60)} deep <script>`);
    expect(container.textContent).toContain("deep");
  });
});
