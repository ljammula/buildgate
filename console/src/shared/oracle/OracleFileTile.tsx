import { ChevronDown, ChevronRight, Circle, CircleCheck } from "lucide-react";
import type { ReactNode } from "react";

import type { OracleFileContent, OracleFileInfo } from "@/domain/oracle";
import { oracleFileSubtitle } from "@/domain/oracle";
import { escapeInvisible } from "@/domain/textEscape";
import type { OracleFileStore } from "@/shared/oracle/useOracleFileStore";
import { OracleContentBox } from "@/shared/oracle/OracleContentBox";
import { ErrorCallout } from "@/ui/ErrorDisplay";
import { Spinner } from "@/ui/Feedback";

export interface OracleFileTileProps {
  /** Identifies the tile in the DOM (`data-testid="oracle-file-<keyId>"`). */
  readonly keyId: string;
  /** The key this file has in `store`. */
  readonly storeKey: string;
  readonly file: OracleFileInfo;
  readonly store: OracleFileStore;
  /** Whether the file currently counts as shown (open, loaded, hash matches the listing). */
  readonly shown: boolean;
  /** Replaces the default content view once content is loaded. */
  readonly renderBody?: (content: OracleFileContent) => ReactNode;
}

/**
 * One file of an oracle directory: name, size, short hash and a shown
 * marker; expanded, its content. Opening it is what makes it "shown".
 */
export function OracleFileTile({
  keyId,
  storeKey,
  file,
  store,
  shown,
  renderBody,
}: OracleFileTileProps) {
  const open = store.isOpen(storeKey);
  const fetched = store.content(storeKey);
  const error = store.error(storeKey);
  const Chevron = open ? ChevronDown : ChevronRight;
  const Mark = shown ? CircleCheck : Circle;
  let body: ReactNode;
  if (error !== undefined) body = <ErrorCallout error={error} />;
  else if (fetched === null) body = <Spinner label="Loading file" />;
  else if (renderBody !== undefined) body = renderBody(fetched.content);
  else body = <OracleContentBox keyId={keyId} content={fetched.content} />;
  return (
    <div data-testid={`oracle-file-${keyId}`} data-shown={shown} className="border-border border-b">
      <button
        type="button"
        aria-expanded={open}
        onClick={() => {
          store.setOpen(storeKey, !open);
        }}
        className="hover:bg-surface-hover flex w-full items-center gap-2 py-1.5 text-left"
      >
        <Chevron className="text-fg-muted size-4 shrink-0" aria-hidden="true" />
        <Mark
          className={shown ? "text-tone-success size-4 shrink-0" : "text-fg-subtle size-4 shrink-0"}
          aria-hidden="true"
        />
        <span className="min-w-0 flex-1">
          <span className="block font-mono text-sm break-all">{escapeInvisible(file.name)}</span>
          <span className="text-fg-muted block font-mono text-xs">{oracleFileSubtitle(file)}</span>
        </span>
      </button>
      {open ? <div className="flex flex-col items-start gap-2 pb-2 pl-6">{body}</div> : null}
    </div>
  );
}
