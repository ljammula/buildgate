import { cn } from "@/ui/cn";

export interface ShortPathProps {
  readonly path: string;
  /** How many trailing segments to keep; the rest shows as a leading "…/". */
  readonly keep?: number;
  readonly className?: string;
}

/**
 * A long absolute path as its last segments, in monospace, with the whole
 * path in the tooltip: the end of a path says which project, the front only
 * says whose home directory.
 */
export function ShortPath({ path, keep = 2, className }: ShortPathProps) {
  const segments = path.split("/").filter((s) => s !== "");
  const short = segments.length > keep ? `…/${segments.slice(-keep).join("/")}` : path;
  return (
    <span title={path} className={cn("block truncate font-mono text-xs", className)}>
      {short}
    </span>
  );
}
