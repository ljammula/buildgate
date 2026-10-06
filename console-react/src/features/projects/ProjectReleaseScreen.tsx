import { useParams } from "react-router";

import { projectReleasePath } from "@/routes/paths";
import { PageBody, PageHeader } from "@/ui/PageLayout";

import { ProjectIdForm } from "./ProjectIdForm";
import { ProjectReleasePanel } from "./ProjectReleasePanel";
import { useLoadProject } from "./useLoadProject";

/**
 * A project's kill-switch state and history by project id alone, at
 * /projects/:project/release: a project with no runs has no other console
 * surface. Strictly observability: engaging and disengaging stay on the
 * command line.
 */
export function ProjectReleaseScreen() {
  const { project = "" } = useParams();
  const load = useLoadProject(project, projectReleasePath, () => undefined);
  return (
    <>
      <PageHeader title="Project release" />
      <PageBody>
        {project === "" ? (
          <ProjectIdForm initial="" loading={false} onLoad={load} />
        ) : (
          <ProjectReleasePanel key={project} project={project} />
        )}
      </PageBody>
    </>
  );
}
