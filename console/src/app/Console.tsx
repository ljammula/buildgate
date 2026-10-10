import { useSyncExternalStore } from "react";

import { App } from "@/app/App";
import { GateScreen } from "@/app/GateScreen";
import type { ConsoleSession } from "@/app/session";

export interface ConsoleProps {
  readonly session: ConsoleSession;
}

/**
 * The app, or the gate screen in its place while the server needs a gate
 * token this browser does not hold. Losing the gate mid-session unmounts the
 * app, which stops its queries and streams: every one of them would be
 * refused.
 */
export function Console({ session }: ConsoleProps) {
  const lost = useSyncExternalStore(session.subscribe, session.gateLost);
  if (lost || session.config.gate === "required") {
    return <GateScreen linkRefused={lost || session.storedTokenRefused} />;
  }
  return <App http={session.http} config={session.config} />;
}
