import * as React from "react";

import { Select } from "@/components/ui/select";
import { Input } from "@/components/ui/input";

// ComboSelect — a real dropdown for a KNOWN, finite option set, with an explicit
// "Custom…" escape for free-text values.
//
// It replaces the `<datalist>` trap (M22 / U2 — the "dropdown not working" bug):
// a native <datalist> FILTERS its suggestions to the input's current text, so a
// pre-filled field appears to have a single option. ComboSelect always shows
// every option; custom entry is opt-in and obvious. Use it wherever the valid
// values are known and finite (models, providers, bindings, …) but a custom
// value must still be possible (apiBase routes, unknown providers).

const CUSTOM = "__custom__";

export function ComboSelect({
  id,
  value,
  options,
  onChange,
  allowCustom = true,
  placeholder = "— select —",
  customPlaceholder = "custom value",
  testId,
}: {
  id?: string;
  value: string;
  options: string[];
  onChange: (v: string) => void;
  /** Allow a free-text value not in `options` (default true). */
  allowCustom?: boolean;
  placeholder?: string;
  customPlaceholder?: string;
  testId?: string;
}) {
  const isKnown = options.includes(value);
  // Custom mode: a non-empty value that isn't a known option (and custom is
  // allowed), or the user explicitly picked "Custom…".
  const [custom, setCustom] = React.useState(
    allowCustom && value !== "" && !isKnown,
  );

  // If the options later include the current value, leave custom mode.
  React.useEffect(() => {
    if (value !== "" && options.includes(value)) setCustom(false);
  }, [options, value]);

  if (allowCustom && custom) {
    return (
      <div className="space-y-1.5">
        <Input
          id={id}
          value={value}
          onChange={(e) => onChange(e.target.value)}
          placeholder={customPlaceholder}
          data-testid={testId}
        />
        {/* The §2.3 inline-link treatment: pine text over a 1.5px pine-surface
            rule that deepens to pine on hover — never a browser underline. */}
        <button
          type="button"
          className="rounded-sm border-b-[1.5px] border-accent text-xs text-primary transition-colors hover:border-primary focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring focus-visible:ring-offset-2"
          onClick={() => {
            setCustom(false);
            onChange("");
          }}
        >
          ← pick from the list
        </button>
      </div>
    );
  }

  return (
    <Select
      id={id}
      value={isKnown ? value : ""}
      data-testid={testId}
      onChange={(e) => {
        if (e.target.value === CUSTOM) {
          setCustom(true);
          onChange("");
          return;
        }
        onChange(e.target.value);
      }}
    >
      <option value="">{placeholder}</option>
      {options.map((o) => (
        <option key={o} value={o}>
          {o}
        </option>
      ))}
      {allowCustom && <option value={CUSTOM}>Custom…</option>}
    </Select>
  );
}
