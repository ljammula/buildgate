import { Search } from "lucide-react";
import { type SyntheticEvent, useState } from "react";
import { useNavigate } from "react-router";

import { isOpenableProjectId } from "@/domain/project";
import { projectsPath } from "@/routes/paths";
import { Button } from "@/ui/Button";
import { Input } from "@/ui/Input";

/**
 * Opens a project by its id on its Release tab, for one the list does not hold: a kill switch
 * can be engaged before a project's first run. Reads nothing itself.
 */
export function ProjectLookupForm() {
  const [id, setId] = useState("");
  const navigate = useNavigate();
  const submit = (event: SyntheticEvent) => {
    event.preventDefault();
    const project = id.trim();
    if (!isOpenableProjectId(project)) return;
    void navigate(projectsPath(project, "release"));
  };
  return (
    <form onSubmit={submit} className="flex items-center gap-2">
      <label htmlFor="project-lookup" className="sr-only">
        Open a project by id
      </label>
      <Input
        id="project-lookup"
        value={id}
        placeholder="Open a project by id"
        className="h-7 w-48 font-mono text-xs"
        onChange={(event) => {
          setId(event.target.value);
        }}
      />
      <Button type="submit" size="sm">
        <Search aria-hidden="true" />
        Open
      </Button>
    </form>
  );
}
