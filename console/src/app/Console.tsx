import { useSyncExternalStore } from "react";

import { App } from "@/app/App";
import { GateScreen } from "@/app/GateScreen";
import type { ConsoleSession } from "@/app/session";

export interface ConsoleProps {
  readonly session: ConsoleSession;
  /** Starts the console again once a pasted gate token was accepted. Injected in tests. */
  readonly restart?: () => void;
}

/**
 * The app, or the gate screen in its place while the server needs a gate
 * token this browser does not hold. Losing the gate mid-session unmounts the
 * app, which stops its queries and streams: every one of them would be
 * refused.
 */
export function Console({ session, restart = reloadPage }: ConsoleProps) {
  const lost = useSyncExternalStore(session.subscribe, session.gateLost);
  if (lost || session.config.gate === "required") {
    return (
      <GateScreen
        tokenRefused={lost || session.storedTokenRefused}
        offerToken={session.offerToken}
        onAccepted={restart}
      />
    );
  }
  return <App http={session.http} config={session.config} />;
}

// The session's Http carries the token it started with, so an accepted
// token takes effect by starting over: startSession reads it from storage.
function reloadPage(): void {
  window.location.reload();
}
