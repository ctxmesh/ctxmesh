import { clsx, type ClassValue } from "clsx";
import { twMerge } from "tailwind-merge";

// cn — the shadcn/ui class-merge helper. Combines conditional classes (clsx)
// and de-duplicates conflicting Tailwind utilities (tailwind-merge). Every
// component uses this so token-based utilities compose predictably.
export function cn(...inputs: ClassValue[]) {
  return twMerge(clsx(inputs));
}
