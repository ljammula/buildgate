import { Check, Copy, Pencil } from "lucide-react";
import { type ReactNode, useEffect, useRef, useState } from "react";

import { EscapedText } from "@/shared/oracle/EscapedText";
import { Button } from "@/ui/Button";
import { Disclosure } from "@/ui/Disclosure";
import { Markdown } from "@/ui/Markdown";
import { useCopied } from "@/ui/useCopied";

import { FileEditor } from "./FileEditor";
import { Panel } from "./Panel";
import type { EditorBinding } from "./useEditSession";

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
  /** The page's edit session for this file; see `useEditSession`. */
  readonly session?: EditorBinding;
  /** Extra content under the text (the parsed acceptance criteria). */
  readonly children?: ReactNode;
  /**
   * Fold the file to a closed disclosure with this one line, for a state in
   * which it is a receipt and no longer what the operator is deciding on
   * (requestDetailLogic.foldsContent). Undefined leaves it open, whole.
   */
  readonly foldedSummary?: string;
}

/** The button that copies the file's real path so the operator can edit it in their own editor. */
function CopyPathButton({ fullPath }: { readonly fullPath: string }) {
  const { copied, copy } = useCopied();
  return (
    <Button
      variant="ghost"
      size="icon"
      aria-label={copied ? "Path copied" : "Copy path to edit this file"}
      onClick={() => void copy(fullPath)}
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
  session,
  children,
  foldedSummary,
}: FileContentSectionProps) {
  const [raw, setRaw] = useState(true);
  // Focus goes back to Edit when the editor closes, not to the page's top.
  const editButton = useRef<HTMLButtonElement>(null);
  const wasEditing = useRef(false);
  useEffect(() => {
    if (wasEditing.current && !editing) editButton.current?.focus();
    wasEditing.current = editing;
  }, [editing]);
  const rawSwitch = (
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
  );
  const pathRow = (
    <div className="flex items-center justify-between gap-2">
      <p className="text-fg-muted min-w-0 truncate font-mono text-xs">
        {fullPath !== "" ? fullPath : path}
      </p>
      {fullPath !== "" ? <CopyPathButton fullPath={fullPath} /> : null}
    </div>
  );
  const text = raw ? (
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
  );
  // Never while editing: an open editor is a decision in progress.
  if (foldedSummary !== undefined && !editing) {
    return (
      <Disclosure title={title} summary={foldedSummary} testId={`content-${path}`}>
        <div className="flex items-center justify-between gap-3">
          <div className="min-w-0 flex-1">{pathRow}</div>
          {rawSwitch}
        </div>
        {text}
      </Disclosure>
    );
  }
  return (
    <Panel
      title={title}
      testId={`content-${path}`}
      actions={
        editing ? null : (
          <>
            {editable ? (
              <Button ref={editButton} size="sm" variant="ghost" onClick={onStartEdit}>
                <Pencil aria-hidden="true" />
                Edit
              </Button>
            ) : null}
            {rawSwitch}
          </>
        )
      }
    >
      {pathRow}
      {editing ? (
        <FileEditor
          path={path}
          initialContent={content}
          onSave={onSave}
          onFetchCurrent={onFetchCurrent}
          onClose={onStopEdit}
          {...(session === undefined ? {} : { session })}
        />
      ) : (
        text
      )}
      {children}
    </Panel>
  );
}
