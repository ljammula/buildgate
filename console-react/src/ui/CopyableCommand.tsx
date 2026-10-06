import { Check, Copy } from "lucide-react";
import { useEffect, useRef, useState } from "react";

import { Button } from "@/ui/Button";
import { cn } from "@/ui/cn";

export interface CopyableCommandProps {
  readonly command: string;
  readonly label?: string;
  readonly writeText?: (text: string) => Promise<void>;
  readonly className?: string;
}

const COPIED_MS = 2000;

function defaultWriteText(text: string): Promise<void> {
  return navigator.clipboard.writeText(text);
}

export function CopyableCommand({
  command,
  label,
  writeText = defaultWriteText,
  className,
}: CopyableCommandProps) {
  const [copied, setCopied] = useState(false);
  const timer = useRef<ReturnType<typeof setTimeout> | undefined>(undefined);

  useEffect(() => () => clearTimeout(timer.current), []);

  async function copy() {
    try {
      await writeText(command);
    } catch {
      return;
    }
    setCopied(true);
    clearTimeout(timer.current);
    timer.current = setTimeout(() => setCopied(false), COPIED_MS);
  }

  return (
    <div className={cn("flex flex-col gap-1", className)}>
      {label ? <span className="text-xs text-fg-muted">{label}</span> : null}
      <div className="flex items-center gap-2 rounded-md border border-border bg-surface-sunken py-1 pr-1 pl-3">
        <code className="min-w-0 flex-1 overflow-x-auto font-mono text-xs whitespace-pre text-fg">
          {command}
        </code>
        <Button
          variant="ghost"
          size="icon"
          aria-label={copied ? "Copied" : "Copy command"}
          onClick={() => void copy()}
        >
          {copied ? <Check aria-hidden="true" /> : <Copy aria-hidden="true" />}
        </Button>
      </div>
    </div>
  );
}
