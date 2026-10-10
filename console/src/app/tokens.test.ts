import { readFileSync } from "node:fs";
import { resolve } from "node:path";

import { describe, expect, it } from "vitest";

import {
  blockAfter,
  composite,
  contrastRatio,
  parseDeclarations,
  parseOklch,
  resolveTokens,
  type Rgba,
} from "@/test/contrast";

const css = readFileSync(resolve(process.cwd(), "src/app/styles.css"), "utf8");

const semantic = parseDeclarations(blockAfter(css, "\n:root {"));
const dark = parseDeclarations(blockAfter(css, "\n:root {\n  color-scheme: dark;"));
const light = parseDeclarations(blockAfter(css, ':root[data-theme="light"] {'));
const lightByMedia = parseDeclarations(
  blockAfter(
    css.slice(css.indexOf("@media (prefers-color-scheme: light)")),
    ":root:not([data-theme]) {",
  ),
);

// A new theme is one line here, next to its block in styles.css.
const themes = [
  ["dark", dark],
  ["light", light],
] as const;

// Pairs that fail today, as "<theme>/<pair id>". Empty this list, never grow it:
// the test fails when a listed pair passes and when an unlisted one fails.
const knownFailures = new Set<string>([]);

interface Pair {
  id: string;
  min: number;
  ratio: (token: (name: string) => Rgba) => number;
}

function pairsFor(theme: "dark" | "light"): Pair[] {
  const pairs: Pair[] = [];
  const plain = (id: string, a: string, b: string, min: number) =>
    pairs.push({ id, min, ratio: (t) => contrastRatio(t(a), t(b)) });
  for (const fg of ["fg", "fg-muted", "fg-subtle"]) {
    for (const bg of ["bg", "surface", "surface-raised", "surface-sunken", "surface-hover"]) {
      plain(`text/${fg}-on-${bg}`, fg, bg, 4.5);
    }
  }
  for (const tone of ["warning", "info", "success", "danger", "neutral"]) {
    pairs.push({
      id: `tone-text/${tone}`,
      min: 4.5,
      ratio: (t) =>
        contrastRatio(t(`tone-${tone}`), composite(t(`tone-${tone}-soft`), t("surface"))),
    });
  }
  plain("accent-text", "accent-fg", "accent", 4.5);
  plain("control-boundary", "border-control", "field", 3.0);
  for (const kind of ["add", "del"]) {
    pairs.push({
      id: `diff-text/${kind}`,
      min: 4.5,
      ratio: (t) =>
        contrastRatio(t(`diff-${kind}-fg`), composite(t(`diff-${kind}`), t("surface-sunken"))),
    });
  }
  // Dark bars are 1.05: the 0.05 flare term of the WCAG formula compresses
  // ratios near black, so 1.10 between four dark steps would force pure black.
  const separation: [string, string, number][] =
    theme === "dark"
      ? [
          ["surface-sunken", "bg", 1.05],
          ["bg", "surface", 1.05],
          ["surface", "surface-raised", 1.05],
          ["surface-raised", "surface-hover", 1.05],
        ]
      : [
          ["bg", "surface", 1.1],
          ["surface-sunken", "surface", 1.1],
          ["surface-hover", "surface", 1.05],
        ];
  for (const [a, b, min] of separation) plain(`separation/${a}:${b}`, a, b, min);
  return pairs;
}

describe("colour conversion", () => {
  // Fixed colours, not tokens: these pin the arithmetic, and must not move
  // when the palette does.
  const ratio = (a: string, b: string) => contrastRatio(parseOklch(a), parseOklch(b));

  it("puts black on white at 21", () => {
    expect(ratio("oklch(0 0 0)", "oklch(1 0 0)")).toBeCloseTo(21, 0);
  });
  it("reproduces independently computed ratios", () => {
    expect(ratio("oklch(0.29 0.012 260)", "oklch(0.19 0.009 260)")).toBeCloseTo(1.31, 1);
    expect(ratio("oklch(0.62 0.014 260)", "oklch(0.25 0.012 260)")).toBeCloseTo(4.39, 1);
    expect(ratio("oklch(0.95 0.005 260)", "oklch(0.155 0.008 260)")).toBeCloseTo(16.9, 1);
    expect(ratio("oklch(0.87 0.007 260)", "oklch(1 0 0)")).toBeCloseTo(1.48, 1);
    expect(ratio("oklch(0.44 0.014 260)", "oklch(0.962 0.004 260)")).toBeCloseTo(7.0, 1);
  });
  it("composites a translucent colour in gamma space", () => {
    // 10% amber over the dark surface, under amber text: 9.0, not the 5.5 a
    // linear-light blend gives.
    const soft = composite(
      parseOklch("oklch(0.83 0.15 80 / 0.1)"),
      parseOklch("oklch(0.19 0.009 260)"),
    );
    expect(contrastRatio(parseOklch("oklch(0.83 0.15 80)"), soft)).toBeCloseTo(9.0, 0);
  });
});

describe("theme tokens", () => {
  it("defines the light theme identically in both places", () => {
    expect([...lightByMedia]).toEqual([...light]);
    expect(light.size).toBeGreaterThan(30);
  });

  it("meets the contrast bars except the known failures", () => {
    const failing = new Set<string>();
    for (const [theme, tokens] of themes) {
      const value = resolveTokens(theme, semantic, tokens);
      const token = (name: string) => parseOklch(value(name));
      for (const pair of pairsFor(theme)) {
        if (pair.ratio(token) < pair.min) failing.add(`${theme}/${pair.id}`);
      }
    }
    expect([...failing].sort()).toEqual([...knownFailures].sort());
  });

  it("names a token a theme lacks", () => {
    expect(() => resolveTokens("x", new Map())("fg")).toThrow("missing token fg in x");
  });

  it("resolves the semantic names through the ramp", () => {
    expect(resolveTokens("dark", semantic, dark)("fg")).toBe(dark.get("n11"));
  });

  // Values worth reading by eye when the palette moves.
  it("reports the ratios the design was tuned to", () => {
    const r = (theme: (typeof themes)[number], a: string, b: string) => {
      const v = resolveTokens(theme[0], semantic, theme[1]);
      return contrastRatio(parseOklch(v(a)), parseOklch(v(b)));
    };
    expect(r(themes[0], "border-control", "field")).toBeCloseTo(3.29, 1);
    expect(r(themes[1], "border-control", "field")).toBeCloseTo(3.64, 1);
  });
});
