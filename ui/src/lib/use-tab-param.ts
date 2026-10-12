import * as React from "react";
import { useSearchParams } from "react-router-dom";

/**
 * The tab a page shows, kept in `?tab=` so a tab is a link. The first tab is the default
 * and is written as no parameter; anything unknown reads as the default. `tabs` must be a
 * module constant.
 */
export function useTabParam<T extends string>(tabs: readonly T[]): [T, (next: T) => void] {
  const [params, setParams] = useSearchParams();
  const raw = params.get("tab");
  const tab = tabs.find((t) => t === raw) ?? tabs[0];
  const setTab = React.useCallback(
    (next: T) =>
      setParams(
        (p) => {
          if (next === tabs[0]) p.delete("tab");
          else p.set("tab", next);
          return p;
        },
        { replace: true },
      ),
    [setParams, tabs],
  );
  return [tab, setTab];
}
