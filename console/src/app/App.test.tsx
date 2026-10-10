import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

import { createHttp } from "@/api/http";
import { App } from "@/app/App";
import { setStoredThemeMode } from "@/platform/themeStore";

function renderApp(path: string, writesEnabled = true) {
  window.history.replaceState(null, "", path);
  const fetch = (() =>
    Promise.resolve(new Response("[]", { status: 200 }))) as typeof globalThis.fetch;
  const http = createHttp({
    baseUrl: "",
    readToken: null,
    startToken: null,
    overrideToken: null,
    gateToken: null,
    fetch,
  });
  return render(
    <App
      http={http}
      config={{ writesEnabled, gate: "off", temporalUiUrl: null, releasePolicyWarning: null }}
    />,
  );
}

afterEach(() => {
  window.history.replaceState(null, "", "/");
  document.documentElement.removeAttribute("data-theme");
  setStoredThemeMode("system");
});

test("the main navigation links to every top-level screen", () => {
  renderApp("/");
  const nav = screen.getByRole("navigation", { name: "Main" });
  const links = Array.from(nav.querySelectorAll("a")).map((a) => [
    a.textContent,
    a.getAttribute("href"),
  ]);
  expect(links).toEqual([
    ["Requests", "/"],
    ["Triage", "/triage"],
    ["Runs", "/runs"],
    ["Projects", "/app/projects"],
    ["Ops", "/ops"],
  ]);
});

test("the current screen's link is marked as the current page", () => {
  renderApp("/runs");
  expect(screen.getByRole("link", { name: "Runs" })).toHaveAttribute("aria-current", "page");
  expect(screen.getByRole("link", { name: "Requests" })).not.toHaveAttribute("aria-current");
});

test("the theme toggle cycles system, light, dark and applies each to the document", async () => {
  renderApp("/");
  const root = document.documentElement;
  await userEvent.click(screen.getByRole("button", { name: "Theme: system (tap for light)" }));
  expect(root).toHaveAttribute("data-theme", "light");
  await userEvent.click(screen.getByRole("button", { name: "Theme: light (tap for dark)" }));
  expect(root).toHaveAttribute("data-theme", "dark");
  await userEvent.click(screen.getByRole("button", { name: "Theme: dark (tap for system)" }));
  expect(root).not.toHaveAttribute("data-theme");
});

test("a console the server accepts no writes from says so", () => {
  renderApp("/", false);
  expect(screen.getByText("Read-only")).toBeInTheDocument();
});
