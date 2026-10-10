import "@/app/styles.css";

import { StrictMode } from "react";
import { createRoot } from "react-dom/client";

import { httpConfigFromEnv } from "@/app/config";
import { Console } from "@/app/Console";
import { startSession } from "@/app/session";
import { captureGateTokenFromLocation } from "@/platform/gateToken";
import { captureStartTokenFromLocation, getStoredStartToken } from "@/platform/startToken";
import { initThemeMode } from "@/platform/theme";

async function start(): Promise<void> {
  // First, before anything can read the URL: take the tokens out of the
  // fragment and strip them from the address bar. The gate token goes first:
  // it removes only its own component, while the start token's capture
  // removes the whole fragment.
  const candidate = captureGateTokenFromLocation();
  captureStartTokenFromLocation();
  initThemeMode();

  const startToken = getStoredStartToken();
  // Tolerant: a server that cannot answer yields a read-only console.
  const session = await startSession({
    candidate,
    httpConfig: (gateToken) => httpConfigFromEnv(startToken, gateToken),
  });

  const root = document.getElementById("root");
  if (!root) throw new Error("index.html has no #root element");
  createRoot(root).render(
    <StrictMode>
      <Console session={session} />
    </StrictMode>,
  );
}

void start();
