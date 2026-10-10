// The new-run intake form, at /app/runs/new.
//
// Pre-fill (the project list's "quick fill", so a project already run against
// is not retyped) arrives in the query string:
//   ?workspace=<path>&spec=<path>&repository=<repository>
// All are optional; none means a blank, from-scratch ("Custom") run.
import { Play } from "lucide-react";
import { type SyntheticEvent } from "react";
import { Link, useNavigate, useSearchParams } from "react-router";

import { useStartRun } from "@/api/runQueries";
import { newRequestPath, runPath } from "@/routes/paths";
import { Button } from "@/ui/Button";
import { Card, CardBody } from "@/ui/Card";
import { Field, Input } from "@/ui/Input";
import { PageBody, PageHeader } from "@/ui/PageLayout";

import { ProjectCheckSection } from "./ProjectCheckSection";
import { StartRunError } from "./StartRunError";
import { newRunFieldLabels, useNewRunForm } from "./useNewRunForm";

export function NewRunScreen() {
  const [params] = useSearchParams();
  const form = useNewRunForm({
    workspace: params.get("workspace") ?? "",
    spec: params.get("spec") ?? "",
    repository: params.get("repository") ?? "",
  });
  const start = useStartRun();
  const navigate = useNavigate();
  const { values } = form;

  const submit = (event: SyntheticEvent) => {
    event.preventDefault();
    if (!form.validate()) return;
    start.mutate(
      {
        ticket: values.ticket.trim(),
        workspace: values.workspace.trim(),
        spec: values.spec.trim(),
        repository: values.repository.trim(),
        temporalAddress: values.temporalAddress.trim(),
      },
      {
        onSuccess: (run) => {
          void navigate(runPath(run.id));
        },
      },
    );
  };

  return (
    <>
      <PageHeader title="New run" />
      <PageBody>
        <Card className="max-w-2xl">
          <CardBody className="p-0">
            <form onSubmit={submit} noValidate className="flex flex-col">
              <div className="border-b border-border px-4 py-3">
                <h2 className="text-sm font-semibold text-fg">Run one ticket</h2>
                <p className="mt-1 text-sm text-fg-muted">
                  Provide the ticket and the repository execution locations. The run will appear in
                  the detail view after factoryd accepts it.
                </p>
                <p className="mt-1 text-sm text-fg-muted">
                  This builds one ticket from a spec that already exists, with no drafting or
                  approval step. To start from a description, use{" "}
                  <Link to={newRequestPath()} className="text-accent hover:underline">
                    New request
                  </Link>
                  .
                </p>
              </div>
              <div className="flex flex-col gap-4 border-b border-border p-4">
                <Field label={newRunFieldLabels.ticket} error={form.errorFor("ticket")}>
                  <Input
                    value={values.ticket}
                    onChange={(e) => {
                      form.set("ticket", e.target.value);
                    }}
                  />
                </Field>
                {/* The project-bootstrap preflight looks for spec/spec.md, spec/contract.md and ARCHITECTURE.md next to this path's PARENT directory, not inside it: the most common cause of a rejected run against a project new to the convention (found live, 2026-09-08). */}
                <Field
                  label={newRunFieldLabels.workspace}
                  hint="A git checkout; its own parent directory must hold spec/spec.md, spec/contract.md, and ARCHITECTURE.md"
                  error={form.errorFor("workspace")}
                >
                  <Input
                    value={values.workspace}
                    onChange={(e) => {
                      form.set("workspace", e.target.value);
                    }}
                  />
                </Field>
                <Field
                  label={newRunFieldLabels.spec}
                  hint="Usually <workspace path>/../spec/spec.md"
                  error={form.errorFor("spec")}
                >
                  <Input
                    value={values.spec}
                    onChange={(e) => {
                      form.set("spec", e.target.value);
                    }}
                  />
                </Field>
                <Field label={newRunFieldLabels.repository} error={form.errorFor("repository")}>
                  <Input
                    value={values.repository}
                    onChange={(e) => {
                      form.set("repository", e.target.value);
                    }}
                  />
                </Field>
              </div>
              <div className="flex flex-col gap-4 border-b border-border p-4">
                <ProjectCheckSection
                  workspace={values.workspace}
                  repository={values.repository}
                  ticket={values.ticket}
                />
                <Field
                  label={newRunFieldLabels.temporalAddress}
                  error={form.errorFor("temporalAddress")}
                >
                  <Input
                    value={values.temporalAddress}
                    placeholder="localhost:7233"
                    onChange={(e) => {
                      form.set("temporalAddress", e.target.value);
                    }}
                  />
                </Field>
              </div>
              <div className="flex flex-col gap-3 bg-surface-sunken/50 p-4">
                {start.error === null ? null : <StartRunError error={start.error} />}
                <div>
                  <Button type="submit" variant="primary" disabled={start.isPending}>
                    <Play aria-hidden="true" />
                    {start.isPending ? "Starting…" : "Start run"}
                  </Button>
                </div>
              </div>
            </form>
          </CardBody>
        </Card>
      </PageBody>
    </>
  );
}
