import * as React from "react";

import { ApiError } from "@/lib/api";

// useLoad — the console's one way to read something and render the three states of
// having asked: still waiting, answered, or refused/failed.
//
// The fetcher receives an AbortSignal and must be stable (wrap it in useCallback):
// a new fetcher identity is a new question, so the hook aborts the old request and
// asks again. A response that arrives after its request was superseded or the
// component unmounted is dropped, never rendered.

/** A read that failed. `status` is the BFF's HTTP status, absent for a network failure. */
export interface LoadFailure {
  kind: "error";
  message: string;
  status?: number;
  /** A 403: render the permission boundary, not a generic error or an empty list. */
  forbidden: boolean;
}

export type Loaded<T> = { kind: "loading" } | { kind: "ready"; data: T } | LoadFailure;

export interface LoadResult<T> {
  state: Loaded<T>;
  /**
   * Ask again. By default the page shows loading and the answer replaces what was on
   * screen. `silent` (a poll, a manual refresh over rows already showing) keeps the
   * current answer visible and keeps it if the refresh fails.
   */
  reload: (opts?: { silent?: boolean }) => void;
  /** True while a silent reload is in flight. */
  refreshing: boolean;
}

/** The failure state for a thrown error. */
export function loadFailure(err: unknown, fallback = "request failed"): LoadFailure {
  if (err instanceof ApiError) {
    return { kind: "error", message: err.message, status: err.status, forbidden: err.isForbidden };
  }
  return {
    kind: "error",
    message: err instanceof Error && err.message ? err.message : fallback,
    forbidden: false,
  };
}

export function useLoad<T>(fetcher: (signal: AbortSignal) => Promise<T>): LoadResult<T> {
  const [state, setState] = React.useState<Loaded<T>>({ kind: "loading" });
  const [refreshing, setRefreshing] = React.useState(false);
  const current = React.useRef<AbortController | null>(null);

  const run = React.useCallback(
    (silent: boolean) => {
      current.current?.abort();
      const ctrl = new AbortController();
      current.current = ctrl;
      setRefreshing(silent);
      if (!silent) setState({ kind: "loading" });
      fetcher(ctrl.signal).then(
        (data) => {
          if (ctrl.signal.aborted) return;
          setState({ kind: "ready", data });
          setRefreshing(false);
        },
        (err: unknown) => {
          if (ctrl.signal.aborted) return;
          // A silent refresh never takes away an answer the reader is looking at. With no
          // answer on screen yet there is nothing to keep, so the failure shows.
          setState((prev) => (silent && prev.kind === "ready" ? prev : loadFailure(err)));
          setRefreshing(false);
        },
      );
    },
    [fetcher],
  );

  React.useEffect(() => {
    run(false);
    return () => current.current?.abort();
  }, [run]);

  // An options object, not a boolean: handed straight to an onClick or onRetry, a
  // boolean parameter would receive the event and read as "silent".
  const reload = React.useCallback(
    (opts?: { silent?: boolean }) => run(opts?.silent === true),
    [run],
  );

  return { state, reload, refreshing };
}
