// WCAG contrast of the console's oklch tokens, for the token test. Colours are
// converted the way a browser paints them: OKLab to linear sRGB, gamut clipped
// by clamping, gamma-encoded, then a translucent colour is blended over the
// opaque one beneath it in encoded sRGB.

export interface Rgba {
  r: number;
  g: number;
  b: number;
  a: number;
}

const oklchPattern = /^oklch\(\s*([\d.]+)\s+([\d.]+)\s+([\d.]+)(?:\s*\/\s*([\d.]+))?\s*\)$/;

function encode(linear: number): number {
  const c = Math.min(1, Math.max(0, linear));
  return c <= 0.0031308 ? 12.92 * c : 1.055 * c ** (1 / 2.4) - 0.055;
}

export function parseOklch(value: string): Rgba {
  const match = oklchPattern.exec(value.trim());
  if (match === null) throw new Error(`not an oklch colour: ${value}`);
  const [l, c, h] = [match[1], match[2], match[3]].map(Number) as [number, number, number];
  const a = match[4] === undefined ? 1 : Number(match[4]);
  const hue = (h * Math.PI) / 180;
  const labA = c * Math.cos(hue);
  const labB = c * Math.sin(hue);
  const l_ = (l + 0.3963377774 * labA + 0.2158037573 * labB) ** 3;
  const m_ = (l - 0.1055613458 * labA - 0.0638541728 * labB) ** 3;
  const s_ = (l - 0.0894841775 * labA - 1.291485548 * labB) ** 3;
  return {
    r: encode(4.0767416621 * l_ - 3.3077115913 * m_ + 0.2309699292 * s_),
    g: encode(-1.2684380046 * l_ + 2.6097574011 * m_ - 0.3413193965 * s_),
    b: encode(-0.0041960863 * l_ - 0.7034186147 * m_ + 1.707614701 * s_),
    a,
  };
}

/** `top` drawn over the opaque `under`. */
export function composite(top: Rgba, under: Rgba): Rgba {
  const mix = (t: number, u: number) => top.a * t + (1 - top.a) * u;
  return { r: mix(top.r, under.r), g: mix(top.g, under.g), b: mix(top.b, under.b), a: 1 };
}

function luminance(color: Rgba): number {
  const lin = (c: number) => (c <= 0.04045 ? c / 12.92 : ((c + 0.055) / 1.055) ** 2.4);
  return 0.2126 * lin(color.r) + 0.7152 * lin(color.g) + 0.0722 * lin(color.b);
}

/** WCAG 2.1 contrast ratio of two opaque colours, 1 to 21. */
export function contrastRatio(a: Rgba, b: Rgba): number {
  const [hi, lo] = [luminance(a), luminance(b)].sort((x, y) => y - x) as [number, number];
  return (hi + 0.05) / (lo + 0.05);
}

/** The `--name: value;` declarations of a block body, in order. */
export function parseDeclarations(body: string): Map<string, string> {
  const out = new Map<string, string>();
  for (const m of body.matchAll(/--([\w-]+)\s*:\s*([^;]+);/g)) {
    out.set(m[1] as string, (m[2] as string).trim());
  }
  return out;
}

/** The body of the first `{ ... }` block that follows `selector` in `css`. */
export function blockAfter(css: string, selector: string): string {
  const start = css.indexOf(selector);
  if (start < 0) throw new Error(`no block for ${selector}`);
  const open = css.indexOf("{", start);
  return css.slice(open + 1, css.indexOf("}", open));
}

/**
 * Every token of a theme by name, with `var(--x)` references resolved: the
 * semantic names point at ramp steps, and a theme's values are the plain
 * `:root` rule overlaid with that theme's own block.
 */
export function resolveTokens(
  theme: string,
  ...layers: ReadonlyMap<string, string>[]
): (name: string) => string {
  const merged = new Map<string, string>();
  for (const layer of layers) for (const [k, v] of layer) merged.set(k, v);
  const resolve = (name: string, depth: number): string => {
    const value = merged.get(name);
    if (value === undefined || depth > 8) throw new Error(`missing token ${name} in ${theme}`);
    const ref = /^var\(--([\w-]+)\)$/.exec(value);
    return ref === null ? value : resolve(ref[1] as string, depth + 1);
  };
  return (name) => resolve(name, 0);
}
