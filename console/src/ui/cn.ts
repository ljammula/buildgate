import { type ClassValue, clsx } from "clsx";
import { twMerge } from "tailwind-merge";

/** Joins class names; a later Tailwind utility overrides an earlier conflicting one. */
export function cn(...inputs: ClassValue[]): string {
  return twMerge(clsx(inputs));
}
