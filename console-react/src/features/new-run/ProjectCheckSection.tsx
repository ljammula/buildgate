import { CircleCheck, CircleX, ClipboardCheck } from "lucide-react";
import { useState } from "react";

import { useCheckProject } from "@/api/runQueries";
import { Button } from "@/ui/Button";
import { ErrorCallout } from "@/ui/ErrorDisplay";

export interface ProjectCheckSectionProps {
  readonly workspace: string;
  readonly repository: string;
  readonly ticket: string;
}

/**
 * Previews the project-bootstrap preflight a run would otherwise discover
 * only by failing closed, so a project new to the convention gets an itemized
 * answer before anything starts. A preview, not a run attempt: it never
 * touches the start state, and only Workspace is required (an operator
 * checking a project has usually not chosen a ticket yet).
 */
export function ProjectCheckSection({ workspace, repository, ticket }: ProjectCheckSectionProps) {
  const check = useCheckProject();
  const [localError, setLocalError] = useState<string | null>(null);

  const run = () => {
    const path = workspace.trim();
    if (path === "") {
      check.reset();
      setLocalError("Workspace path is required to check a project");
      return;
    }
    setLocalError(null);
    check.mutate({ workspace: path, repository: repository.trim(), ticket: ticket.trim() });
  };

  const error = localError ?? check.error;
  const result = check.data;
  return (
    <div className="flex flex-col gap-2">
      <div>
        <Button disabled={check.isPending} onClick={run}>
          <ClipboardCheck aria-hidden="true" />
          {check.isPending ? "Checking…" : "Check project setup"}
        </Button>
      </div>
      {error === null ? null : (
        // POST /projects/check is start-token-gated.
        <ErrorCallout error={error} startClass />
      )}
      {result === undefined ? null : (
        <div className="flex flex-col gap-1 text-sm">
          <p className={result.passed ? "font-semibold text-fg" : "font-semibold text-tone-danger"}>
            {result.passed ? "Project setup looks ready." : "Project setup is not ready yet:"}
          </p>
          {/* Every declared check, not only the failing ones: an operator who just fixed one artifact wants to see the others passing. */}
          <ul className="flex flex-col gap-1">
            {result.checks.map((item) => (
              <li key={`${item.check}-${item.path}`} className="flex items-start gap-1.5">
                {item.passed ? (
                  <CircleCheck aria-hidden="true" className="mt-0.5 size-4 text-tone-success" />
                ) : (
                  <CircleX aria-hidden="true" className="mt-0.5 size-4 text-tone-danger" />
                )}
                <span className="font-mono text-xs">
                  {item.passed
                    ? `${item.check} (${item.path})`
                    : `${item.check} (${item.path}): ${item.reasons.join("; ")}`}
                </span>
              </li>
            ))}
          </ul>
        </div>
      )}
    </div>
  );
}
