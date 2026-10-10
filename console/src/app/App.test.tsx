import { render, screen, waitFor } from "@testing-library/react";
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
    ["Mission Control", "/"],
    ["Triage", "/triage"],
    ["Runs", "/runs"],
    ["Projects", "/app/projects"],
  ]);
});

test("the current screen's link is marked as the current page", () => {
  renderApp("/runs");
  expect(screen.getByRole("link", { name: "Runs" })).toHaveAttribute("aria-current", "page");
  expect(screen.getByRole("link", { name: "Mission Control" })).not.toHaveAttribute("aria-current");
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

test.each([
  ["/ops", "/app/projects"],
  ["/projects/app/stats", "/app/projects?project=app&tab=stats"],
  ["/projects/app/release", "/app/projects?project=app&tab=release"],
  ["/projects/app/observations", "/app/projects?project=app&tab=observations"],
  ["/projects/my%20repo/release", "/app/projects?project=my+repo&tab=release"],
])("the retired page %s redirects to %s", async (from, to) => {
  renderApp(from);
  await waitFor(() => {
    expect(window.location.pathname + window.location.search).toBe(to);
  });
  expect(await screen.findByRole("heading", { name: "Projects" })).toBeInTheDocument();
});

test("a retired page replaces its history entry, so Back does not return to it", async () => {
  renderApp("/projects/app/release");
  await waitFor(() => {
    expect(window.location.pathname).toBe("/app/projects");
  });
  expect(window.history.state).toMatchObject({ idx: 0 });
});
