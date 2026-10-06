import { useEffect, useReducer } from "react";

import { useApi } from "@/api/ApiProvider";
import { watchRunLog } from "@/api/runs";
import { appendLog } from "@/features/run-detail/logBuffer";

interface LogState {
  readonly text: string;
  readonly error: unknown;
  readonly closed: boolean;
  /** Bumped by `retry`, so the subscription effect runs again. */
  readonly attempt: number;
}

type LogAction =
  | { readonly type: "chunk"; readonly chunk: string }
  | { readonly type: "error"; readonly error: unknown }
  | { readonly type: "done" }
  | { readonly type: "clear" }
  | { readonly type: "retry" };

const initialLog: LogState = { text: "", error: null, closed: false, attempt: 0 };

function reduceLog(state: LogState, action: LogAction): LogState {
  switch (action.type) {
    case "chunk":
      return { ...state, text: appendLog(state.text, action.chunk) };
    case "error":
      return { ...state, error: action.error };
    case "done":
      return { ...state, closed: true };
    case "clear":
      return { ...initialLog, attempt: state.attempt };
    case "retry":
      return { ...initialLog, attempt: state.attempt + 1 };
  }
}

export interface RunLog {
  readonly text: string;
  readonly error: unknown;
  /** The server closed the stream (or it failed): nothing more will arrive. */
  readonly closed: boolean;
  readonly retry: () => void;
}

/**
 * The tail of a run's build log, subscribed only while `enabled`: off by
 * default and opt-in per run, so a hundred open tabs are not a hundred active
 * `GET /runs/{id}/log?follow=1` streams. Turning it off unsubscribes and
 * clears the text; turning it on again starts from the full log, which is
 * what the server returns on every new request.
 */
export function useRunLog(id: string, enabled: boolean): RunLog {
  const { http } = useApi();
  const [state, dispatch] = useReducer(reduceLog, initialLog);

  useEffect(() => {
    if (!enabled) return;
    const unsubscribe = watchRunLog(http, id, {
      onChunk: (chunk) => {
        dispatch({ type: "chunk", chunk });
      },
      onError: (error) => {
        dispatch({ type: "error", error });
      },
      onDone: () => {
        dispatch({ type: "done" });
      },
    });
    return () => {
      unsubscribe();
      dispatch({ type: "clear" });
    };
  }, [http, id, enabled, state.attempt]);

  return {
    text: state.text,
    error: state.error,
    closed: state.closed,
    retry: () => {
      dispatch({ type: "retry" });
    },
  };
}
