import "@/app/styles.css";

import { StrictMode } from "react";
import { createRoot } from "react-dom/client";

import { createHttp } from "@/api/http";
import { fetchConsoleConfig } from "@/api/ops";
import { App } from "@/app/App";
import { httpConfigFromEnv } from "@/app/config";
import { captureStartTokenFromLocation, getStoredStartToken } from "@/platform/startToken";
import { initThemeMode } from "@/platform/theme";

async function start(): Promise<void> {
  // First, before anything can read the URL: move a start token out of the
  // fragment and into storage, and strip it from the address bar.
  captureStartTokenFromLocation();
  initThemeMode();

  const http = createHttp(httpConfigFromEnv(getStoredStartToken()));
  // Tolerant: a server that cannot answer yields a read-only console.
  const config = await fetchConsoleConfig(http);

  const root = document.getElementById("root");
  if (!root) throw new Error("index.html has no #root element");
  createRoot(root).render(
    <StrictMode>
      <App http={http} config={config} />
    </StrictMode>,
  );
}

void start();
