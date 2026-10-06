import { type SyntheticEvent, useState } from "react";

import { Button } from "@/ui/Button";
import { Field, Input } from "@/ui/Input";

export interface ProjectIdFormProps {
  /** The id the page currently shows (empty before one is chosen). */
  readonly initial: string;
  /** Disabled while a lookup is in flight: a second one could land out of order. */
  readonly loading: boolean;
  readonly onLoad: (project: string) => void;
}

/** The project id field and its Load button: the id `factoryd kill-switch -project` takes. */
export function ProjectIdForm({ initial, loading, onLoad }: ProjectIdFormProps) {
  const [value, setValue] = useState(initial);
  const project = value.trim();
  const submit = (event: SyntheticEvent) => {
    event.preventDefault();
    if (loading || project === "") return;
    onLoad(project);
  };
  return (
    <form onSubmit={submit} className="flex items-end gap-3">
      <Field label="Project id" className="max-w-md flex-1">
        <Input
          value={value}
          placeholder="the id factoryd kill-switch -project takes"
          onChange={(event) => {
            setValue(event.target.value);
          }}
        />
      </Field>
      <Button type="submit" variant="primary" disabled={loading}>
        Load
      </Button>
    </form>
  );
}
