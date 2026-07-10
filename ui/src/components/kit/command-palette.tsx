import * as React from "react";
import { CornerDownLeft, Search } from "lucide-react";
import type { LucideIcon } from "lucide-react";

import { cn } from "@/lib/utils";

// CommandPalette — the cmd-K navigator (kit, m13.1; spec §5). Open with ⌘K /
// Ctrl-K, type to fuzzy-filter across navigation targets, recent agents, and
// quick actions ("Connect a provider", "Describe an agent"); Enter runs the
// selected item. This is the fast path power users expect and a strong
// anti-"rudimentary" signal.
//
// Controlled `open`; the parent wires the ⌘K listener (via `useCommandK`) and
// supplies grouped `commands`. Keyboard: ↑/↓ move, Enter runs, Esc closes.

export interface CommandItem {
  id: string;
  label: string;
  icon?: LucideIcon;
  /** Group heading (e.g. "Navigate", "Actions", "Recent agents"). */
  group?: string;
  /** Extra searchable keywords not shown in the label. */
  keywords?: string;
  hint?: string;
  onRun: () => void;
}

export interface CommandPaletteProps {
  open: boolean;
  onClose: () => void;
  commands: CommandItem[];
  placeholder?: string;
}

/** Wire ⌘K / Ctrl-K to toggle a palette. Returns [open, setOpen]. */
export function useCommandK(): [boolean, React.Dispatch<React.SetStateAction<boolean>>] {
  const [open, setOpen] = React.useState(false);
  React.useEffect(() => {
    function onKey(e: KeyboardEvent) {
      if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === "k") {
        e.preventDefault();
        setOpen((v) => !v);
      }
    }
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, []);
  return [open, setOpen];
}

export function CommandPalette({
  open,
  onClose,
  commands,
  placeholder = "Search or jump to…",
}: CommandPaletteProps) {
  const [q, setQ] = React.useState("");
  const [active, setActive] = React.useState(0);
  const inputRef = React.useRef<HTMLInputElement>(null);

  React.useEffect(() => {
    if (open) {
      setQ("");
      setActive(0);
      // Focus the input on open without a lint-flagged autoFocus attribute.
      inputRef.current?.focus();
    }
  }, [open]);

  const filtered = React.useMemo(() => {
    const needle = q.trim().toLowerCase();
    if (!needle) return commands;
    return commands.filter((c) =>
      `${c.label} ${c.group ?? ""} ${c.keywords ?? ""}`
        .toLowerCase()
        .includes(needle),
    );
  }, [commands, q]);

  // Group in stable order, preserving first-seen group sequence.
  const groups = React.useMemo(() => {
    const map = new Map<string, CommandItem[]>();
    for (const c of filtered) {
      const g = c.group ?? "";
      if (!map.has(g)) map.set(g, []);
      map.get(g)!.push(c);
    }
    return Array.from(map.entries());
  }, [filtered]);

  function onKeyDown(e: React.KeyboardEvent) {
    if (e.key === "ArrowDown") {
      e.preventDefault();
      setActive((a) => Math.min(a + 1, filtered.length - 1));
    } else if (e.key === "ArrowUp") {
      e.preventDefault();
      setActive((a) => Math.max(a - 1, 0));
    } else if (e.key === "Enter") {
      e.preventDefault();
      const item = filtered[active];
      if (item) {
        item.onRun();
        onClose();
      }
    } else if (e.key === "Escape") {
      onClose();
    }
  }

  if (!open) return null;

  let flatIndex = -1;

  return (
    <div
      className="fixed inset-0 z-50 flex items-start justify-center p-4 pt-[12vh]"
      role="dialog"
      aria-modal="true"
      aria-label="Command palette"
    >
      <div
        className="absolute inset-0 bg-foreground/40 backdrop-blur-[2px]"
        onClick={onClose}
        aria-hidden="true"
      />
      <div className="relative w-full max-w-xl overflow-hidden rounded-xl border bg-popover shadow-overlay">
        <div className="flex items-center gap-3 border-b px-4">
          <Search className="h-4 w-4 text-muted-foreground" />
          <input
            ref={inputRef}
            value={q}
            onChange={(e) => {
              setQ(e.target.value);
              setActive(0);
            }}
            onKeyDown={onKeyDown}
            placeholder={placeholder}
            aria-label="Command"
            className="h-12 w-full bg-transparent text-sm outline-none placeholder:text-muted-foreground"
          />
          <kbd className="hidden rounded border bg-surface-2 px-1.5 py-0.5 text-[10px] font-medium text-muted-foreground sm:inline">
            ESC
          </kbd>
        </div>

        <div className="max-h-[22rem] overflow-y-auto p-2">
          {filtered.length === 0 && (
            <p className="px-3 py-8 text-center text-sm text-muted-foreground">
              No commands match “{q}”.
            </p>
          )}
          {groups.map(([group, items]) => (
            <div key={group || "_"} className="mb-1">
              {group && (
                <p className="px-3 pb-1 pt-2 text-[10px] font-semibold uppercase tracking-wide text-muted-foreground">
                  {group}
                </p>
              )}
              {items.map((item) => {
                flatIndex += 1;
                const isActive = flatIndex === active;
                const Icon = item.icon;
                return (
                  <button
                    key={item.id}
                    type="button"
                    onMouseEnter={() => setActive(flatIndex)}
                    onClick={() => {
                      item.onRun();
                      onClose();
                    }}
                    className={cn(
                      "flex w-full items-center gap-3 rounded-md px-3 py-2 text-left text-sm transition-colors",
                      isActive
                        ? "bg-accent text-accent-foreground"
                        : "text-foreground hover:bg-surface-2",
                    )}
                  >
                    {Icon && <Icon className="h-4 w-4 text-muted-foreground" />}
                    <span className="flex-1 truncate">{item.label}</span>
                    {item.hint && (
                      <span className="text-xs text-muted-foreground">
                        {item.hint}
                      </span>
                    )}
                    {isActive && (
                      <CornerDownLeft className="h-3.5 w-3.5 text-muted-foreground" />
                    )}
                  </button>
                );
              })}
            </div>
          ))}
        </div>
      </div>
    </div>
  );
}
