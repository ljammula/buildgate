import { Check, Copy, Pencil } from "lucide-react";
import { type ReactNode, useEffect, useRef, useState } from "react";

import { EscapedText } from "@/shared/oracle/EscapedText";
import { Button } from "@/ui/Button";
import { Markdown } from "@/ui/Markdown";

import { FileEditor } from "./FileEditor";
import { Panel } from "./Panel";

export interface FileContentSectionProps {
  readonly title: string;
  /** The request-relative path, e.g. "spec.md" or "tickets/001.spec.md". */
  readonly path: string;
  /** Home-relativized real path on the serve host; empty on a server predating it. */
  readonly fullPath: string;
  /**
   * The content EXACTLY as shown: the approval hashes this same string, so
   * the raw and rendered views and the editor's starting text all read it.
   */
  readonly content: string;
  /** Whether Edit is offered: only in the review state the file's PUT route accepts, and with write access. */
  readonly editable: boolean;
  readonly editing: boolean;
  readonly onStartEdit: () => void;
  readonly onStopEdit: () => void;
  readonly onSave: (content: string, baseSha256: string) => Promise<void>;
  readonly onFetchCurrent: () => Promise<string>;
  /** Extra content under the text (the parsed acceptance criteria). */
  readonly children?: ReactNode;
}

const COPIED_MS = 2000;

/** The button that copies the file's real path so the operator can edit it in their own editor. */
function CopyPathButton({ fullPath }: { readonly fullPath: string }) {
  const [copied, setCopied] = useState(false);
  const timer = useRef<ReturnType<typeof setTimeout> | undefined>(undefined);
  useEffect(
    () => () => {
      clearTimeout(timer.current);
    },
    [],
  );
  async function copy() {
    try {
      await navigator.clipboard.writeText(fullPath);
    } catch {
      return;
    }
    setCopied(true);
    clearTimeout(timer.current);
    timer.current = setTimeout(() => {
      setCopied(false);
    }, COPIED_MS);
  }
  return (
    <Button
      variant="ghost"
      size="icon"
      aria-label={copied ? "Path copied" : "Copy path to edit this file"}
      onClick={() => void copy()}
    >
      {copied ? <Check aria-hidden="true" /> : <Copy aria-hidden="true" />}
    </Button>
  );
}

/**
 * One spec or plan file: its path, a Raw/Rendered switch (raw by default,
 * the literal text the hash covers), and an Edit control where the state
 * allows one. Spec and plan text is agent-written: the raw view writes
 * hidden characters out, the rendered view never makes HTML.
 */
export function FileContentSection({
  title,
  path,
  fullPath,
  content,
  editable,
  editing,
  onStartEdit,
  onStopEdit,
  onSave,
  onFetchCurrent,
  children,
}: FileContentSectionProps) {
  const [raw, setRaw] = useState(true);
  return (
    <Panel
      title={title}
      testId={`content-${path}`}
      actions={
        editing ? null : (
          <>
            {editable ? (
              <Button size="sm" variant="ghost" onClick={onStartEdit}>
                <Pencil aria-hidden="true" />
                Edit
              </Button>
            ) : null}
            <label className="text-fg-muted flex items-center gap-1.5 text-xs">
              Raw
              <input
                type="checkbox"
                role="switch"
                checked={raw}
                onChange={(event) => {
                  setRaw(event.target.checked);
                }}
              />
            </label>
          </>
        )
      }
    >
      <div className="flex items-center justify-between gap-2">
        <p className="text-fg-muted min-w-0 truncate font-mono text-xs">
          {fullPath !== "" ? fullPath : path}
        </p>
        {fullPath !== "" ? <CopyPathButton fullPath={fullPath} /> : null}
      </div>
      {editing ? (
        <FileEditor
          path={path}
          initialContent={content}
          onSave={onSave}
          onFetchCurrent={onFetchCurrent}
          onClose={onStopEdit}
        />
      ) : raw ? (
        <div
          data-testid="markdown-raw-content"
          className="border-border bg-surface-sunken max-h-[32rem] overflow-auto rounded-md border p-3"
        >
          <EscapedText text={content} className="font-mono text-xs" />
        </div>
      ) : (
        <div
          data-testid="markdown-rendered-content"
          className="border-border max-h-[32rem] overflow-auto rounded-md border p-3"
        >
          <Markdown source={content} />
        </div>
      )}
      {children}
    </Panel>
  );
}
