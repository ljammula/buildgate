import { type SyntheticEvent, type ReactNode, useState } from "react";
import { useNavigate } from "react-router";

import { Button } from "@/ui/Button";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
  DialogTrigger,
} from "@/ui/Dialog";
import { Field, Input } from "@/ui/Input";

export interface ProjectLookupDialogProps {
  /** The dialog's title and the trigger's name: "Project release" or "Project stats". */
  readonly title: string;
  readonly trigger: ReactNode;
  /** The page for a project id (a routes/paths builder). */
  readonly pathFor: (project: string) => string;
}

/**
 * Reaches a project's page by its id alone: the console surface a project with
 * no runs otherwise has no way to reach. The id is the one
 * `factoryd kill-switch -project` takes.
 */
export function ProjectLookupDialog({ title, trigger, pathFor }: ProjectLookupDialogProps) {
  const [open, setOpen] = useState(false);
  const [project, setProject] = useState("");
  const navigate = useNavigate();
  const id = project.trim();
  const submit = (event: SyntheticEvent) => {
    event.preventDefault();
    if (id === "") return;
    setOpen(false);
    void navigate(pathFor(id));
  };
  return (
    <Dialog open={open} onOpenChange={setOpen}>
      <DialogTrigger asChild>{trigger}</DialogTrigger>
      <DialogContent>
        <form onSubmit={submit} className="flex flex-col gap-4">
          <DialogHeader>
            <DialogTitle>{title}</DialogTitle>
            <DialogDescription>Open a project by its id.</DialogDescription>
          </DialogHeader>
          <Field label="Project id">
            <Input
              value={project}
              placeholder="the id factoryd kill-switch -project takes"
              onChange={(event) => {
                setProject(event.target.value);
              }}
            />
          </Field>
          <DialogFooter>
            <Button type="submit" variant="primary" disabled={id === ""}>
              Open
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}
