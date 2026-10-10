import { useRef } from "react";

import { useRunDiff } from "@/api/runQueries";
import { changedFileLines } from "@/features/run-detail/diffFiles";
import { Button } from "@/ui/Button";
import { DiffView } from "@/ui/DiffView";
import { ErrorCallout } from "@/ui/ErrorDisplay";
import { Spinner } from "@/ui/Feedback";

/**
 * The full unified diff between a run's base and result SHAs: real
 * visibility into what a run changed, beyond the file-count summary on the
 * Overview. Fetched only once this view is selected. A chip per changed
 * file, read from the diff's own headers, scrolls the diff to that file.
 */
export function DiffPanel({ runId }: { runId: string }) {
  const diff = useRunDiff(runId);
  const body = useRef<HTMLDivElement>(null);

  function jumpTo(line: number) {
    const row = body.current?.querySelector("pre")?.children.item(line);
    if (row && typeof row.scrollIntoView === "function") row.scrollIntoView({ block: "start" });
  }

  let content;
  if (diff.data !== undefined) {
    const files = changedFileLines(diff.data.diff.split("\n"));
    content = (
      <div ref={body} className="flex flex-col gap-3">
        {files.length > 0 ? (
          <ul aria-label="Changed files" className="flex flex-wrap gap-2">
            {files.map((file) => (
              <li key={file.line}>
                <Button
                  size="sm"
                  className="font-mono text-xs"
                  onClick={() => {
                    jumpTo(file.line);
                  }}
                >
                  {file.name}
                </Button>
              </li>
            ))}
          </ul>
        ) : null}
        <DiffView diff={diff.data.diff} truncated={diff.data.truncated} />
      </div>
    );
  } else if (diff.isError) {
    content = <ErrorCallout error={diff.error} />;
  } else {
    content = <Spinner label="Loading diff" />;
  }
  return <div className="flex flex-col gap-3">{content}</div>;
}
