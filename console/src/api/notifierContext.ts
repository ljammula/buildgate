import { createContext, useContext } from "react";

/**
 * True while this tab is the one raising browser notifications. The request
 * event streams read it and say so (`?notifier=1`), so the server holds back
 * the host's own desktop banner while a tab does the job.
 */
export const NotifierContext = createContext(false);

export function useIsNotifier(): boolean {
  return useContext(NotifierContext);
}
