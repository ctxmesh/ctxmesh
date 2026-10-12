import * as React from "react";
import { describe, expect, it } from "vitest";
import { act, renderHook, waitFor } from "@testing-library/react";

import { ApiError } from "@/lib/api";
import { loadFailure, useLoad } from "@/lib/use-load";

/** A promise the test settles by hand, so each state can be observed in between. */
function deferred<T>() {
  let resolve!: (v: T) => void;
  let reject!: (e: unknown) => void;
  const promise = new Promise<T>((res, rej) => {
    resolve = res;
    reject = rej;
  });
  return { promise, resolve, reject };
}

/** The shape a page uses: a fetcher keyed on what it reads, here a namespace. */
function useNamespacedLoad(ns: string, ask: (ns: string, signal: AbortSignal) => Promise<string>) {
  const fetcher = React.useCallback((signal: AbortSignal) => ask(ns, signal), [ns, ask]);
  return useLoad(fetcher);
}

describe("useLoad", () => {
  it("is loading until the fetcher answers, then ready with its data", async () => {
    const d = deferred<string[]>();
    const fetcher = () => d.promise;
    const { result } = renderHook(() => useLoad(fetcher));
    expect(result.current.state).toEqual({ kind: "loading" });
    act(() => d.resolve(["a", "b"]));
    await waitFor(() => expect(result.current.state).toEqual({ kind: "ready", data: ["a", "b"] }));
  });

  it("reports a 403 as forbidden, with its status and message", async () => {
    const fetcher = () => Promise.reject(new ApiError("forbidden: no list", 403));
    const { result } = renderHook(() => useLoad(fetcher));
    await waitFor(() =>
      expect(result.current.state).toEqual({
        kind: "error",
        message: "forbidden: no list",
        status: 403,
        forbidden: true,
      }),
    );
  });

  it("keeps the status of any other failure, so a page can tell a 404 or a 501 apart", async () => {
    const fetcher = () => Promise.reject(new ApiError("not wired", 501));
    const { result } = renderHook(() => useLoad(fetcher));
    await waitFor(() => expect(result.current.state).toMatchObject({ kind: "error", status: 501, forbidden: false }));
  });

  it("reports a network failure without a status", async () => {
    const fetcher = () => Promise.reject(new TypeError("Failed to fetch"));
    const { result } = renderHook(() => useLoad(fetcher));
    await waitFor(() =>
      expect(result.current.state).toEqual({ kind: "error", message: "Failed to fetch", forbidden: false }),
    );
  });

  it("asks again on reload, showing loading in between", async () => {
    let n = 0;
    const gates = [deferred<number>(), deferred<number>()];
    const fetcher = () => gates[n++].promise;
    const { result } = renderHook(() => useLoad(fetcher));
    act(() => gates[0].resolve(1));
    await waitFor(() => expect(result.current.state).toEqual({ kind: "ready", data: 1 }));

    act(() => result.current.reload());
    expect(result.current.state).toEqual({ kind: "loading" });
    act(() => gates[1].resolve(2));
    await waitFor(() => expect(result.current.state).toEqual({ kind: "ready", data: 2 }));
  });

  it("a failed reload replaces the answer, so Retry after an error clears it", async () => {
    let fail = true;
    const fetcher = () => (fail ? Promise.reject(new Error("down")) : Promise.resolve("up"));
    const { result } = renderHook(() => useLoad(fetcher));
    await waitFor(() => expect(result.current.state).toMatchObject({ kind: "error", message: "down" }));
    fail = false;
    act(() => result.current.reload());
    await waitFor(() => expect(result.current.state).toEqual({ kind: "ready", data: "up" }));
  });

  it("a silent reload keeps the answer on screen and flags refreshing until it lands", async () => {
    let n = 0;
    const gates = [deferred<string>(), deferred<string>()];
    const fetcher = () => gates[n++].promise;
    const { result } = renderHook(() => useLoad(fetcher));
    act(() => gates[0].resolve("first"));
    await waitFor(() => expect(result.current.state).toEqual({ kind: "ready", data: "first" }));

    act(() => result.current.reload({ silent: true }));
    expect(result.current.state).toEqual({ kind: "ready", data: "first" });
    expect(result.current.refreshing).toBe(true);
    act(() => gates[1].resolve("second"));
    await waitFor(() => expect(result.current.state).toEqual({ kind: "ready", data: "second" }));
    expect(result.current.refreshing).toBe(false);
  });

  it("a failed silent reload keeps the answer it had", async () => {
    let n = 0;
    const fetcher = () => (n++ === 0 ? Promise.resolve("kept") : Promise.reject(new Error("blip")));
    const { result } = renderHook(() => useLoad(fetcher));
    await waitFor(() => expect(result.current.state).toEqual({ kind: "ready", data: "kept" }));
    act(() => result.current.reload({ silent: true }));
    await waitFor(() => expect(result.current.refreshing).toBe(false));
    expect(result.current.state).toEqual({ kind: "ready", data: "kept" });
  });

  it("a failed silent reload with nothing on screen yet shows the failure", async () => {
    const fetcher = () => Promise.reject(new Error("never answered"));
    const { result } = renderHook(() => useLoad(fetcher));
    await waitFor(() => expect(result.current.state.kind).toBe("error"));
    act(() => result.current.reload({ silent: true }));
    await waitFor(() => expect(result.current.refreshing).toBe(false));
    expect(result.current.state).toMatchObject({ kind: "error", message: "never answered" });
  });

  it("treats an event handed to reload as a plain reload, not a silent one", async () => {
    let fail = true;
    const fetcher = () => (fail ? Promise.reject(new Error("down")) : Promise.resolve("up"));
    const { result } = renderHook(() => useLoad(fetcher));
    await waitFor(() => expect(result.current.state.kind).toBe("error"));
    fail = false;
    act(() => (result.current.reload as (e: unknown) => void)({ type: "click", target: {} }));
    expect(result.current.state).toEqual({ kind: "loading" });
    await waitFor(() => expect(result.current.state).toEqual({ kind: "ready", data: "up" }));
  });

  it("asks again when the fetcher changes, and drops the answer to the old question", async () => {
    const old = deferred<string>();
    const fresh = deferred<string>();
    const signals: AbortSignal[] = [];
    const ask = (ns: string, signal: AbortSignal) => {
      signals.push(signal);
      return ns === "team-a" ? old.promise : fresh.promise;
    };
    const { result, rerender } = renderHook(({ ns }: { ns: string }) => useNamespacedLoad(ns, ask), {
      initialProps: { ns: "team-a" },
    });
    rerender({ ns: "team-b" });
    expect(signals[0].aborted).toBe(true);
    act(() => fresh.resolve("team-b rows"));
    await waitFor(() => expect(result.current.state).toEqual({ kind: "ready", data: "team-b rows" }));
    await act(async () => {
      old.resolve("team-a rows");
      await old.promise;
      await new Promise((r) => setTimeout(r, 0));
    });
    expect(result.current.state).toEqual({ kind: "ready", data: "team-b rows" });
  });

  it("aborts the request on unmount", () => {
    const signals: AbortSignal[] = [];
    const fetcher = (signal: AbortSignal) => {
      signals.push(signal);
      return new Promise<never>(() => {});
    };
    const { unmount } = renderHook(() => useLoad(fetcher));
    unmount();
    expect(signals.at(-1)?.aborted).toBe(true);
  });
});

describe("loadFailure", () => {
  it("falls back to the given sentence for an error with no message", () => {
    expect(loadFailure("boom", "couldn't load the route")).toEqual({
      kind: "error",
      message: "couldn't load the route",
      forbidden: false,
    });
  });
});
