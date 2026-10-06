// The module boundaries of this console are lint rules, so a violation fails
// `npm run lint` instead of waiting for a reviewer. README.md has the
// dependency diagram these rules implement.
import { readdirSync } from "node:fs";

import js from "@eslint/js";
import reactHooks from "eslint-plugin-react-hooks";
import globals from "globals";
import tseslint from "typescript-eslint";

const layers = ["domain", "api", "platform", "routes", "ui", "shared", "features", "app"];

// What each layer may import from the others. A layer always may import
// itself.
const allowed = {
  domain: [],
  platform: ["domain"],
  routes: ["domain"],
  api: ["domain"],
  ui: ["domain"],
  shared: ["domain", "api", "platform", "routes", "ui"],
  features: ["domain", "api", "platform", "routes", "ui", "shared"],
  app: ["domain", "api", "platform", "routes", "ui", "shared", "features"],
};

// A cross-directory import goes through the "@/" alias, so the patterns
// below see every boundary crossing.
const noParentImports = {
  group: ["../*"],
  message: 'Import across directories through the "@/" alias, so the layer rules can check it.',
};

function layerRule(layer) {
  const banned = layers.filter((other) => other !== layer && !allowed[layer].includes(other));
  return {
    files: [`src/${layer}/**/*.{ts,tsx}`],
    rules: {
      "no-restricted-imports": [
        "error",
        {
          patterns: [
            noParentImports,
            ...banned.map((other) => ({
              group: [`@/${other}`, `@/${other}/**`],
              message: `${layer}/ may not import ${other}/ (see README.md, "Module layout").`,
            })),
            ...(layer === "domain"
              ? [
                  {
                    group: ["react", "react-dom", "react-router", "@tanstack/*"],
                    message: "domain/ is pure TypeScript: no React, no I/O.",
                  },
                ]
              : []),
          ],
        },
      ],
    },
  };
}

// features/X never imports features/Y: it navigates through routes/ link
// builders.
function featureRules() {
  let names = [];
  try {
    names = readdirSync(new URL("./src/features", import.meta.url), { withFileTypes: true })
      .filter((entry) => entry.isDirectory())
      .map((entry) => entry.name);
  } catch {
    // No features yet.
  }
  return names.map((name) => ({
    files: [`src/features/${name}/**/*.{ts,tsx}`],
    rules: {
      "no-restricted-imports": [
        "error",
        {
          patterns: [
            noParentImports,
            { group: ["@/app", "@/app/**"], message: "features/ may not import app/." },
            {
              group: ["@/features/**", `!@/features/${name}`, `!@/features/${name}/**`],
              message:
                "A feature may not import another feature: navigate with a routes/ link builder.",
            },
          ],
        },
      ],
    },
  }));
}

const untrustedHtml = [
  {
    selector: "JSXAttribute[name.name='dangerouslySetInnerHTML']",
    message:
      "Agent-written text is untrusted: render it through ui/Markdown or as text, never as HTML.",
  },
  {
    selector: "MemberExpression[property.name=/^(innerHTML|outerHTML)$/]",
    message: "Agent-written text is untrusted: never build DOM from an HTML string.",
  },
  {
    selector: "CallExpression[callee.property.name='insertAdjacentHTML']",
    message: "Agent-written text is untrusted: never build DOM from an HTML string.",
  },
  { selector: "NewExpression[callee.name='Function']", message: "No code from strings." },
];

export default tseslint.config(
  { ignores: ["dist", "node_modules", "coverage"] },
  js.configs.recommended,
  ...tseslint.configs.strictTypeChecked,
  {
    languageOptions: {
      globals: globals.browser,
      parserOptions: { projectService: true, tsconfigRootDir: import.meta.dirname },
    },
    plugins: { "react-hooks": reactHooks },
    rules: {
      ...reactHooks.configs.recommended.rules,
      "no-eval": "error",
      "no-implied-eval": "off",
      "@typescript-eslint/no-implied-eval": "error",
      "no-restricted-syntax": ["error", ...untrustedHtml],
      "no-restricted-globals": [
        "error",
        { name: "fetch", message: "Only src/api/ talks HTTP." },
        { name: "localStorage", message: "Only src/platform/ touches browser storage." },
        { name: "sessionStorage", message: "Only src/platform/ touches browser storage." },
        {
          name: "EventSource",
          message: "Streams are fetched with an Authorization header in src/api/sse.ts.",
        },
      ],
      "no-restricted-properties": [
        "error",
        { object: "window", property: "fetch", message: "Only src/api/ talks HTTP." },
        { object: "globalThis", property: "fetch", message: "Only src/api/ talks HTTP." },
        {
          object: "window",
          property: "localStorage",
          message: "Only src/platform/ touches browser storage.",
        },
        {
          object: "globalThis",
          property: "localStorage",
          message: "Only src/platform/ touches browser storage.",
        },
      ],
      "@typescript-eslint/consistent-type-imports": "error",
      "@typescript-eslint/restrict-template-expressions": ["error", { allowNumber: true }],
    },
  },
  ...layers.map(layerRule),
  ...featureRules(),
  {
    files: ["src/api/**/*.{ts,tsx}"],
    rules: {
      "no-restricted-globals": [
        "error",
        { name: "localStorage", message: "Only src/platform/ touches browser storage." },
        { name: "sessionStorage", message: "Only src/platform/ touches browser storage." },
        {
          name: "EventSource",
          message: "Streams are fetched with an Authorization header in src/api/sse.ts.",
        },
      ],
      "no-restricted-properties": "off",
    },
  },
  {
    files: ["src/platform/**/*.{ts,tsx}"],
    rules: {
      "no-restricted-globals": ["error", { name: "fetch", message: "Only src/api/ talks HTTP." }],
      "no-restricted-properties": "off",
    },
  },
  {
    // Tests and config run in Node and read fixture files.
    files: ["src/**/*.test.{ts,tsx}", "src/test/**", "*.config.{ts,js}"],
    languageOptions: { globals: { ...globals.node, ...globals.vitest } },
    rules: {
      "@typescript-eslint/no-non-null-assertion": "off",
      "@typescript-eslint/no-unsafe-assignment": "off",
      "@typescript-eslint/no-unsafe-member-access": "off",
      "@typescript-eslint/no-unsafe-argument": "off",
    },
  },
  { files: ["eslint.config.js"], ...tseslint.configs.disableTypeChecked },
  {
    // The Node scripts of the live browser walk: plain JavaScript outside
    // the TypeScript project, and free to use fetch.
    files: ["test/**/*.mjs"],
    ...tseslint.configs.disableTypeChecked,
    languageOptions: {
      ...tseslint.configs.disableTypeChecked.languageOptions,
      globals: { ...globals.node, ...globals.browser },
    },
    rules: {
      ...tseslint.configs.disableTypeChecked.rules,
      "no-restricted-globals": "off",
      "no-restricted-properties": "off",
    },
  },
);
