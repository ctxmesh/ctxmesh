import type { ReactNode } from "react";

import { Label } from "@/components/ui/label";

// FormField — a labelled form control with an optional hint + inline validation
// message. Pure layout over the token primitives; the error text uses the
// destructive token (no hardcoded color). Every config-builder field composes it
// so labels/spacing/error styling are uniform.
export function FormField({
  id,
  label,
  hint,
  error,
  children,
}: {
  id: string;
  label: string;
  hint?: string;
  error?: string;
  children: ReactNode;
}) {
  return (
    <div className="space-y-1.5">
      <Label htmlFor={id}>{label}</Label>
      {children}
      {hint && !error && (
        <p className="text-xs text-muted-foreground">{hint}</p>
      )}
      {error && (
        <p className="text-xs text-destructive" role="alert">
          {error}
        </p>
      )}
    </div>
  );
}
