import { ChevronDown, ChevronRight, Loader2, Send } from "lucide-react";
import { useId, useState } from "react";

import { useApi } from "@/api/ApiProvider";
import { useWorkspaces } from "@/api/runQueries";
import { OperatorGate } from "@/shared/approval/OperatorGate";
import { Button } from "@/ui/Button";
import { Card } from "@/ui/Card";
import { ErrorCallout } from "@/ui/ErrorDisplay";
import { Callout, Spinner } from "@/ui/Feedback";
import { Checkbox, Field, Input, Select, Textarea } from "@/ui/Input";
import { PageBody, PageHeader } from "@/ui/PageLayout";

import { useNewRequestForm } from "./useNewRequestForm";

/**
 * Starts a request (POST /requests): the higher-level entry point that
 * drives spec drafting, planning and building, the same thing
 * `factoryd submit` starts from a terminal.
 */
export function NewRequestScreen() {
  const { canWrite } = useApi();
  const workspaces = useWorkspaces();
  const form = useNewRequestForm();
  const [advanced, setAdvanced] = useState(false);
  // The dropdown's selection is kept apart from the text field: a pick
  // mirrors into the field, but the operator can still hand-edit a path the
  // listing never offered.
  const [selected, setSelected] = useState("");
  const draftId = useId();
  const { values, set, errors } = form;

  const hints = workspaces.data ?? [];
  const currentHint = hints.find((hint) => hint.workspace === values.workspace.trim()) ?? null;
  const verifyDefault =
    currentHint !== null && currentHint.resolvedVerifyCommand !== ""
      ? `Default: ${currentHint.resolvedVerifyCommand}`
      : "Default: this workspace's .factory.yml verify_command";

  return (
    <>
      <PageHeader
        title="New request"
        description="A request drives spec drafting, planning, and building end to end -- the same thing `factoryd submit` starts from a terminal."
      />
      <PageBody>
        <Card className="max-w-2xl">
          <form
            noValidate
            className="flex flex-col"
            onSubmit={(event) => {
              event.preventDefault();
              form.submit();
            }}
          >
            <div className="border-b border-border px-4 py-3">
              <h2 className="text-base font-semibold text-fg">Start a request</h2>
            </div>
            <div className="flex flex-col gap-4 border-b border-border p-4">
              {workspaces.isPending ? <Spinner label="Loading workspaces" /> : null}
              {workspaces.isError ? <ErrorCallout error={workspaces.error} /> : null}
              {hints.length > 0 ? (
                <Field label="Known workspace">
                  <Select
                    value={selected}
                    onChange={(event) => {
                      setSelected(event.target.value);
                      if (event.target.value !== "") set({ workspace: event.target.value });
                    }}
                  >
                    <option value="">Pick one, or type a path below</option>
                    {hints.map((hint) => (
                      <option key={hint.workspace} value={hint.workspace}>
                        {hint.workspace}
                      </option>
                    ))}
                  </Select>
                </Field>
              ) : null}
              <Field
                label="Workspace path"
                error={errors.workspace}
                hint={
                  currentHint?.hasFactoryYml ? (
                    <span>
                      {currentHint.resolvedVerifyCommand !== ""
                        ? `Verify command from ${currentHint.verifyCommandSource}: ${currentHint.resolvedVerifyCommand}`
                        : "This workspace has a .factory.yml, but no verify_command set."}
                    </span>
                  ) : undefined
                }
              >
                <Input
                  className="font-mono"
                  placeholder="A git repository root on the factoryd host"
                  value={values.workspace}
                  onChange={(event) => {
                    set({ workspace: event.target.value });
                  }}
                />
              </Field>
            </div>
            <div className="flex flex-col gap-4 border-b border-border p-4">
              <Field label="Request" error={errors.text}>
                <Textarea
                  rows={8}
                  placeholder="What should the factory build?"
                  value={values.text}
                  onChange={(event) => {
                    set({ text: event.target.value });
                  }}
                />
              </Field>
              <div className="flex items-start gap-2">
                <Checkbox
                  id={draftId}
                  aria-describedby={`${draftId}-desc`}
                  className="mt-0.5"
                  checked={values.draftOracles}
                  onChange={(event) => {
                    set({ draftOracles: event.target.checked });
                  }}
                />
                <div className="flex flex-col">
                  <label htmlFor={draftId} className="text-sm font-medium text-fg">
                    Draft oracles
                  </label>
                  <p id={`${draftId}-desc`} className="text-xs text-fg-subtle">
                    Adds an oracle-review stage between spec approval and planning, to
                    approve/hand-write/skip acceptance-test oracles before any ticket is planned.
                  </p>
                </div>
              </div>
            </div>
            <div className="flex flex-col gap-3 border-b border-border px-4 py-3">
              <Button
                variant="ghost"
                size="sm"
                className="self-start"
                aria-expanded={advanced}
                onClick={() => {
                  setAdvanced((open) => !open);
                }}
              >
                {advanced ? (
                  <ChevronDown aria-hidden="true" />
                ) : (
                  <ChevronRight aria-hidden="true" />
                )}
                Advanced
              </Button>
              {advanced ? (
                <>
                  <Field label="Verify command">
                    <Input
                      className="font-mono"
                      placeholder={verifyDefault}
                      value={values.verifyCommand}
                      onChange={(event) => {
                        set({ verifyCommand: event.target.value });
                      }}
                    />
                  </Field>
                  <Field label="Full-suite command">
                    <Input
                      className="font-mono"
                      placeholder="Repo-wide regression command; default: none, or .factory.yml full_suite_command"
                      value={values.fullSuiteCommand}
                      onChange={(event) => {
                        set({ fullSuiteCommand: event.target.value });
                      }}
                    />
                  </Field>
                  <Field label="Preflight profile">
                    <Input
                      placeholder={'Empty (strict), or "brownfield"'}
                      value={values.preflightProfile}
                      onChange={(event) => {
                        set({ preflightProfile: event.target.value });
                      }}
                    />
                  </Field>
                </>
              ) : null}
            </div>
            <div className="flex flex-col gap-3 bg-surface-sunken/50 p-4">
              {!canWrite ? (
                <Callout tone="danger">
                  This console cannot write to the server from here -- no override token is
                  configured and the server did not enable an unauthenticated console write.
                </Callout>
              ) : null}
              {form.error ? <ErrorCallout error={form.error} /> : null}
              <Button
                type="submit"
                variant="primary"
                className="self-start"
                disabled={form.submitting || !canWrite}
              >
                {form.submitting ? (
                  <Loader2 className="animate-spin" aria-hidden="true" />
                ) : (
                  <Send aria-hidden="true" />
                )}
                {form.submitting ? "Submitting…" : "Submit request"}
              </Button>
            </div>
          </form>
        </Card>
      </PageBody>
      {form.askName ? (
        <OperatorGate onOpenChange={form.cancelName} onNamed={form.confirmName} />
      ) : null}
    </>
  );
}
