import { useParams } from "react-router";

import { projectObservationsPath } from "@/routes/paths";
import { PageBody, PageHeader } from "@/ui/PageLayout";

import { ProjectIdForm } from "./ProjectIdForm";
import { ProjectObservationsPanel } from "./ProjectObservationsPanel";
import { useLoadProject } from "./useLoadProject";

/**
 * What a project's finished runs have shown, by project id alone, at
 * /projects/:project/observations. Strictly observability.
 */
export function ProjectObservationsScreen() {
  const { project = "" } = useParams();
  const load = useLoadProject(project, projectObservationsPath, () => undefined);
  return (
    <>
      <PageHeader title="Project observations" />
      <PageBody>
        {project === "" ? (
          <ProjectIdForm initial="" loading={false} onLoad={load} />
        ) : (
          <ProjectObservationsPanel key={project} project={project} />
        )}
      </PageBody>
    </>
  );
}
