import * as React from "react";
import { useNavigate } from "react-router-dom";
import { Command } from "lucide-react";

import { CommandPalette, useCommandK } from "@/components/kit";
import { useCapabilities } from "@/lib/capabilities";
import { useNamespace } from "@/lib/namespace";
import { buildCommands } from "@/lib/palette-commands";
import { cn } from "@/lib/utils";

// command-palette-shell - wires the kit's cmd-K palette into the PRODUCTION shell
// (m13.6b). The kit ships the primitive (CommandPalette + useCommandK); this is
// the shell adapter that (1) owns the global cmd-K / Ctrl-K binding, (2) builds
// the command groups from live app state (lib/palette-commands), and (3)
// RBAC-FILTERS them EXACTLY like the nav (the same `requiresCapability` capability
// gate the Sidebar uses, ui-foundation section 3, DISPLAY-ONLY per ADR 0011) so
// a viewer's palette never lists write-only surfaces they can't reach. It mounts
// INSIDE the Namespace + Capabilities providers (ShellChrome) so it reads the
// same context the nav does.
//
// The palette is composed AS-IS - no kit fork. Esc / backdrop close and all
// keyboard-first behaviour (arrow-key nav, Enter, fuzzy filter, focus-return)
// come from the kit. This adapter only supplies `commands` + wires open/close.

// PaletteTrigger is the discoverable cmd-K affordance in the top bar: a chip that
// shows the shortcut AND opens the palette on click (keyboard users get the
// global binding; pointer users get the chip). Purely a second opener - the
// source of truth for open-state is the parent's useCommandK.
//
// It is the SECOND thing to give way as the bar narrows (M151 section 4.2's
// collapse order: identity, then this, then the workspace switcher). It gives way
// in two steps rather than one: below `lg` the word and the keycap go and the
// chip becomes a 32px icon button; below `md` it leaves the bar entirely, since a
// touch keyboard has no cmd-K to advertise and the drawer carries navigation
// there. The BINDING never collapses - it is on window, at every width.
function PaletteTrigger({ onOpen }: { onOpen: () => void }) {
  return (
    <button
      type="button"
      onClick={onOpen}
      aria-label="Open command palette"
      aria-keyshortcuts="Meta+K Control+K"
      title="Search or jump to - Cmd K"
      className={cn(
        "hidden h-8 shrink-0 items-center justify-center gap-2 rounded-md border border-border-strong bg-card",
        "text-xs text-faint transition-colors hover:bg-surface-2 hover:text-foreground",
        "focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring focus-visible:ring-offset-2",
        "w-8 px-0 md:inline-flex lg:w-auto lg:px-2.5",
      )}
    >
      <Command className="h-3.5 w-3.5 shrink-0" aria-hidden="true" />
      <span className="hidden lg:inline">Search</span>
      <kbd className="hidden items-center gap-0.5 rounded-sm border bg-surface-2 px-1 font-mono text-2xs lg:flex">
        <Command className="h-2.5 w-2.5" aria-hidden="true" />K
      </kbd>
    </button>
  );
}

// ShellCommandPalette is the mounted, RBAC-aware palette. It lives inside the
// shell providers and receives the shell's logout handler (so sign-out from the
// palette runs the exact same flow as the header button - toast + redirect).
export function ShellCommandPalette({ onLogout }: { onLogout: () => void }) {
  const [open, setOpen] = useCommandK();
  const navigate = useNavigate();
  const { can } = useCapabilities();
  const { namespace, setNamespace, list } = useNamespace();

  const namespaces = React.useMemo(
    () => (list.kind === "ready" ? list.namespaces : []),
    [list],
  );
  // Stable key so the command memo only rebuilds when the namespace SET changes.
  // Include display names in the key so renaming a namespace refreshes the palette.
  const namespacesKey = namespaces.map((n) => `${n.name}:${n.displayName ?? ""}`).join(" ");

  const commands = React.useMemo(
    () =>
      buildCommands({
        can,
        navigate,
        namespaces,
        currentNamespace: namespace,
        setNamespace,
        onLogout,
      }),
    // `can` is recreated when the capability state changes, so it (plus the
    // namespace inputs) is the correct dependency set - the palette re-filters
    // when RBAC resolves or the namespace list loads. namespacesKey stands in
    // for the `namespaces` array identity.
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [can, navigate, namespacesKey, namespace, setNamespace, onLogout],
  );

  return (
    <>
      <PaletteTrigger onOpen={() => setOpen(true)} />
      <CommandPalette
        open={open}
        onClose={() => setOpen(false)}
        commands={commands}
        placeholder="Search or jump to"
      />
    </>
  );
}
