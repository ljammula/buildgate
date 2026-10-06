import { cn } from "@/ui/cn";
import { CopyButton } from "@/ui/CopyButton";

export interface CopyableCommandProps {
  readonly command: string;
  readonly label?: string;
  readonly writeText?: (text: string) => Promise<void>;
  readonly className?: string;
}

export function CopyableCommand({ command, label, writeText, className }: CopyableCommandProps) {
  return (
    <div className={cn("flex flex-col gap-1", className)}>
      {label ? <span className="text-xs text-fg-muted">{label}</span> : null}
      <div className="flex items-center gap-2 rounded-md border border-border bg-surface-sunken py-1 pr-1 pl-3">
        <code className="min-w-0 flex-1 overflow-x-auto font-mono text-xs whitespace-pre text-fg">
          {command}
        </code>
        <CopyButton text={command} label="Copy command" writeText={writeText} />
      </div>
    </div>
  );
}
