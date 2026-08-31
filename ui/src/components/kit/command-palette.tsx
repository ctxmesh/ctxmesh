import * as React from "react";
import { CornerDownLeft, Search } from "lucide-react";
import type { LucideIcon } from "lucide-react";

import { cn } from "@/lib/utils";
import { useFocusTrap } from "@/components/kit/use-focus-trap";

// CommandPalette — the cmd-K navigator (kit, m13.1 → real m13.4; spec §5). Open
// with ⌘K / Ctrl-K, type to fuzzy-filter across navigation targets, recent
// agents, and quick actions ("Connect a provider", "Describe an agent"); Enter
// runs the selected item. This is the fast path power users expect and a strong
// anti-"rudimentary" signal.
//
// Controlled `open`; the parent wires the ⌘K listener (via `useCommandK`) and
// supplies grouped `commands` (navigation-ready — the parent passes command
// groups). Keyboard-first: ↑/↓ move the active item (wrapping + scroll-into-
// view), Enter runs it, Esc closes. Productionized in m13.4: subsequence fuzzy
// matching, a labelled active option (aria-activedescendant), focus-return to
// the opener on close, and the active row scrolled into view on keyboard move.

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

// Subsequence fuzzy match: every needle char appears in order in the haystack
// (e.g. "gta" → "Go to Agents"). Falls back naturally to substring matches.
function fuzzyMatch(needle: string, haystack: string): boolean {
  if (!needle) return true;
  let i = 0;
  for (let j = 0; j < haystack.length && i < needle.length; j++) {
    if (haystack[j] === needle[i]) i++;
  }
  return i === needle.length;
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
  const listRef = React.useRef<HTMLDivElement>(null);
  // Focus trap: keeps Tab inside the palette, returns focus to the opener on
  // close, and owns Esc. Focus lands on the input (first focusable) on open.
  const panelRef = useFocusTrap<HTMLDivElement>({ active: open, onEscape: onClose });

  React.useEffect(() => {
    if (open) {
      setQ("");
      setActive(0);
    }
  }, [open]);

  const filtered = React.useMemo(() => {
    const needle = q.trim().toLowerCase();
    if (!needle) return commands;
    return commands.filter((c) =>
      fuzzyMatch(
        needle,
        `${c.label} ${c.group ?? ""} ${c.keywords ?? ""}`.toLowerCase(),
      ),
    );
  }, [commands, q]);

  // Keep the active row visible as ↑/↓ moves it past the scroll viewport.
  React.useEffect(() => {
    const el = listRef.current?.querySelector<HTMLElement>(
      '[data-active="true"]',
    );
    // scrollIntoView is a browser nicety (jsdom lacks it) — guard the call.
    el?.scrollIntoView?.({ block: "nearest" });
  }, [active, filtered]);

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

  // Esc is owned by the focus trap; here we handle only list navigation + run.
  function onKeyDown(e: React.KeyboardEvent) {
    if (filtered.length === 0) return;
    if (e.key === "ArrowDown") {
      e.preventDefault();
      setActive((a) => (a + 1) % filtered.length); // wrap to top
    } else if (e.key === "ArrowUp") {
      e.preventDefault();
      setActive((a) => (a - 1 + filtered.length) % filtered.length); // wrap to bottom
    } else if (e.key === "Enter") {
      e.preventDefault();
      const item = filtered[active];
      if (item) {
        item.onRun();
        onClose();
      }
    }
  }

  if (!open) return null;

  const activeId = filtered[active] ? `cmd-${filtered[active].id}` : undefined;
  let flatIndex = -1;

  return (
    <div
      ref={panelRef}
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
          <Search className="h-4 w-4 text-faint" />
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
            role="combobox"
            aria-expanded="true"
            aria-controls="cmdk-list"
            aria-activedescendant={activeId}
            className="h-12 w-full bg-transparent text-sm outline-none placeholder:font-mono placeholder:text-ghost"
          />
          <kbd className="hidden rounded-sm border bg-surface-2 px-1.5 py-0.5 font-mono text-2xs text-faint sm:inline">
            ESC
          </kbd>
        </div>

        <div
          id="cmdk-list"
          ref={listRef}
          role="listbox"
          aria-label="Commands"
          className="max-h-[22rem] overflow-y-auto p-2"
        >
          {filtered.length === 0 && (
            <p className="px-3 py-8 text-center text-sm text-muted-foreground">
              No commands match “{q}”.
            </p>
          )}
          {groups.map(([group, items]) => (
            <div key={group || "_"} className="mb-1">
              {group && (
                <p className="px-3 pb-1 pt-2 font-mono text-2xs uppercase tracking-wide text-faint">
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
                    id={`cmd-${item.id}`}
                    role="option"
                    aria-selected={isActive}
                    data-active={isActive}
                    type="button"
                    tabIndex={-1}
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
                    {Icon && <Icon className="h-4 w-4 text-faint" />}
                    <span className="flex-1 truncate">{item.label}</span>
                    {item.hint && (
                      <span className="font-mono text-xs text-faint">
                        {item.hint}
                      </span>
                    )}
                    {isActive && (
                      <CornerDownLeft className="h-3.5 w-3.5 text-faint" />
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
