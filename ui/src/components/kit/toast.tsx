import * as React from "react";
import { Check, Info, TriangleAlert, X } from "lucide-react";

import { Button } from "@/components/ui/button";
import { cn } from "@/lib/utils";

// Toast — transient confirmation with an optional Undo (kit, m13.1 → real
// m13.4; spec §5). Reversible actions (delete, disable) show a toast with Undo
// so the console stays forgiving at scale.
//
// Two layers ship here:
//   • <Toast> — the presentational card (title / description / Undo / dismiss).
//   • <ToastProvider> + useToast() — the m13.4 system: a host that stacks
//     toasts in an aria-live region, auto-dismisses after a timeout, and gives
//     surfaces a one-call `toast(...)` API. The Undo affordance runs the
//     caller's `onUndo` and dismisses immediately (the classic delete-with-undo
//     pattern: the toast IS the grace window before the write commits).

export type ToastVariant = "success" | "error" | "info";

export interface ToastProps {
  variant?: ToastVariant;
  title: React.ReactNode;
  description?: React.ReactNode;
  /** Reversible action affordance. */
  onUndo?: () => void;
  onDismiss?: () => void;
  className?: string;
}

const ICONS: Record<ToastVariant, typeof Check> = {
  success: Check,
  error: TriangleAlert,
  info: Info,
};

// Icon tone (M151 §5.14). `info` is deliberately NOT a hue: the --info slot now
// carries the hold violet ("a person must decide", ADR 0128), and a neutral
// notice has no status to annotate — so its icon is plain ink.
const TONE: Record<ToastVariant, string> = {
  success: "text-success",
  error: "text-destructive",
  info: "text-foreground",
};

export function Toast({
  variant = "info",
  title,
  description,
  onUndo,
  onDismiss,
  className,
}: ToastProps) {
  const Icon = ICONS[variant];
  return (
    <div
      role="status"
      className={cn(
        "flex w-full max-w-sm items-start gap-3 rounded-lg border bg-popover p-4 shadow-elevated",
        className,
      )}
    >
      <Icon className={cn("mt-0.5 h-5 w-5 shrink-0", TONE[variant])} />
      <div className="min-w-0 flex-1">
        <p className="text-sm font-medium">{title}</p>
        {description && (
          <p className="mt-0.5 text-sm text-muted-foreground">{description}</p>
        )}
      </div>
      {onUndo && (
        <Button variant="link" size="sm" className="h-auto p-0" onClick={onUndo}>
          Undo
        </Button>
      )}
      {onDismiss && (
        <button
          type="button"
          onClick={onDismiss}
          aria-label="Dismiss"
          className="rounded-sm text-faint transition-colors hover:text-foreground focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring focus-visible:ring-offset-2"
        >
          <X className="h-4 w-4" />
        </button>
      )}
    </div>
  );
}

// ── Toast system (provider + hook) ────────────────────────────────────────

export interface ToastOptions {
  variant?: ToastVariant;
  title: React.ReactNode;
  description?: React.ReactNode;
  /** Reversible-action callback. When present, the toast shows "Undo". */
  onUndo?: () => void;
  /** Auto-dismiss after N ms. 0 = sticky (never auto-dismiss). Default 5000. */
  duration?: number;
}

interface ActiveToast extends ToastOptions {
  id: number;
}

export interface ToastContextValue {
  /** Enqueue a toast; returns its id (so a caller can dismiss it early). */
  toast: (opts: ToastOptions) => number;
  dismiss: (id: number) => void;
}

const ToastContext = React.createContext<ToastContextValue | null>(null);

/** Default auto-dismiss window — also the Undo grace window for reversibles. */
export const DEFAULT_TOAST_DURATION = 5000;

export function ToastProvider({ children }: { children: React.ReactNode }) {
  const [toasts, setToasts] = React.useState<ActiveToast[]>([]);
  const idRef = React.useRef(0);
  // Track live timers so a manual/undo dismiss cancels the pending auto-dismiss.
  const timers = React.useRef(new Map<number, ReturnType<typeof setTimeout>>());

  const dismiss = React.useCallback((id: number) => {
    const t = timers.current.get(id);
    if (t) {
      clearTimeout(t);
      timers.current.delete(id);
    }
    setToasts((prev) => prev.filter((x) => x.id !== id));
  }, []);

  const toast = React.useCallback(
    (opts: ToastOptions) => {
      const id = ++idRef.current;
      const duration = opts.duration ?? DEFAULT_TOAST_DURATION;
      setToasts((prev) => [...prev, { ...opts, id }]);
      if (duration > 0) {
        timers.current.set(
          id,
          setTimeout(() => dismiss(id), duration),
        );
      }
      return id;
    },
    [dismiss],
  );

  // Clear any outstanding timers if the provider unmounts.
  React.useEffect(() => {
    const map = timers.current;
    return () => {
      map.forEach((t) => clearTimeout(t));
      map.clear();
    };
  }, []);

  const value = React.useMemo<ToastContextValue>(
    () => ({ toast, dismiss }),
    [toast, dismiss],
  );

  return (
    <ToastContext.Provider value={value}>
      {children}
      {/* Host — a fixed, polite live region stacking the active toasts. */}
      <div
        aria-live="polite"
        aria-atomic="false"
        className="pointer-events-none fixed bottom-4 right-4 z-[100] flex w-full max-w-sm flex-col gap-2"
      >
        {toasts.map((t) => (
          <div key={t.id} className="pointer-events-auto">
            <Toast
              variant={t.variant}
              title={t.title}
              description={t.description}
              onUndo={
                t.onUndo
                  ? () => {
                      t.onUndo?.();
                      dismiss(t.id);
                    }
                  : undefined
              }
              onDismiss={() => dismiss(t.id)}
            />
          </div>
        ))}
      </div>
    </ToastContext.Provider>
  );
}

/** Access the toast API. Must be called under a <ToastProvider>. */
export function useToast(): ToastContextValue {
  const ctx = React.useContext(ToastContext);
  if (!ctx) {
    throw new Error("useToast must be used within a <ToastProvider>");
  }
  return ctx;
}
