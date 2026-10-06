import { useEffect, useRef, useState } from "react";
import { useNavigate } from "react-router";

export interface LeaveGuard {
  /** True while the page is asking whether to leave. */
  readonly asking: boolean;
  /** Leave anyway: runs the navigation that was held back. */
  readonly leave: () => void;
  /** Stay on the page. */
  readonly stay: () => void;
}

const guardKey = "buildgateLeaveGuard";

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null;
}

function guardState(base: unknown): Record<string, unknown> {
  return { ...(isRecord(base) ? base : {}), [guardKey]: true };
}

/**
 * Asks before the page is left while `armed` (unsaved text). The app runs
 * under `BrowserRouter`, which has no navigation blockers (`useBlocker` needs
 * a data router), so each way out is caught where it happens:
 * - closing or reloading the tab: `beforeunload`;
 * - an in-app link (Back to board, the sidebar): a capture-phase click listener
 *   holds the navigation and replays it on Leave;
 * - the browser's Back button: a guard history entry sits above the page's own,
 *   so Back lands on the page again and the question is asked; Leave then goes
 *   back past both.
 */
export function useLeaveGuard(armed: boolean): LeaveGuard {
  const navigate = useNavigate();
  const [held, setHeld] = useState<{ readonly run: () => void } | null>(null);
  const leaving = useRef(false);

  useEffect(() => {
    if (!armed) return;
    const onBeforeUnload = (event: BeforeUnloadEvent) => {
      event.preventDefault();
      // eslint-disable-next-line @typescript-eslint/no-deprecated -- some browsers only prompt when it is set
      event.returnValue = "";
    };
    window.addEventListener("beforeunload", onBeforeUnload);
    return () => {
      window.removeEventListener("beforeunload", onBeforeUnload);
    };
  }, [armed]);

  useEffect(() => {
    if (!armed) return;
    const onClick = (event: MouseEvent) => {
      if (event.defaultPrevented || event.button !== 0) return;
      if (event.metaKey || event.ctrlKey || event.shiftKey || event.altKey) return;
      const anchor = event.target instanceof Element ? event.target.closest("a[href]") : null;
      if (!(anchor instanceof HTMLAnchorElement)) return;
      if (anchor.target !== "" && anchor.target !== "_self") return;
      const href = anchor.getAttribute("href") ?? "";
      // An in-app path only; "#..." stays on the page and anything else is a real unload.
      if (!href.startsWith("/") || href.startsWith("//")) return;
      event.preventDefault();
      event.stopPropagation();
      setHeld({
        run: () => {
          leaving.current = true;
          // Replace the guard entry: Back from the next page then lands on this one, once.
          void navigate(href, { replace: true });
        },
      });
    };
    document.addEventListener("click", onClick, true);
    return () => {
      document.removeEventListener("click", onClick, true);
    };
  }, [armed, navigate]);

  useEffect(() => {
    if (!armed) return;
    leaving.current = false;
    const entry = window.history.state as unknown;
    window.history.pushState(guardState(entry), "");
    const onPop = () => {
      // Back landed on the page's own entry: restore the guard entry above it, then ask.
      window.history.pushState(guardState(entry), "");
      setHeld({
        run: () => {
          leaving.current = true;
          window.removeEventListener("popstate", onPop);
          window.history.go(-2);
        },
      });
    };
    window.addEventListener("popstate", onPop);
    return () => {
      window.removeEventListener("popstate", onPop);
      // Saved or discarded in place: take the guard entry away so Back is one press again.
      if (
        !leaving.current &&
        isRecord(window.history.state) &&
        window.history.state[guardKey] === true
      ) {
        window.history.go(-1);
      }
    };
  }, [armed]);

  return {
    asking: held !== null,
    leave: () => {
      held?.run();
      setHeld(null);
    },
    stay: () => {
      setHeld(null);
    },
  };
}
