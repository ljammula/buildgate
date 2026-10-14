import { cn } from "@/ui/cn";
import { CopyButton } from "@/ui/CopyButton";

export interface PathValueProps {
  readonly path: string;
  /** What the copy button names, e.g. "workspace path". */
  readonly copyLabel?: string;
  readonly writeText?: (text: string) => Promise<void>;
  readonly className?: string;
}

/** A complete filesystem path, wrapped to fit, with an optional copy control. */
export function PathValue({ path, copyLabel, writeText, className }: PathValueProps) {
  const text = (
    <span title={path} className={cn("block min-w-0 break-all font-mono text-xs", className)}>
      {path}
    </span>
  );
  if (copyLabel === undefined || path === "") return text;
  return (
    <span className="inline-flex min-w-0 max-w-full items-start gap-1">
      {text}
      <CopyButton
        size="sm"
        kind="path"
        text={path}
        label={`Copy ${copyLabel}`}
        writeText={writeText}
      />
    </span>
  );
}
