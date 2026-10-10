import { useParams } from "react-router";

import { projectStatsPath } from "@/routes/paths";
import { PageBody, PageHeader } from "@/ui/PageLayout";

import { ProjectIdForm } from "./ProjectIdForm";
import { ProjectStatsPanel } from "./ProjectStatsPanel";
import { useLoadProject } from "./useLoadProject";

/**
 * A project's acceptance/override-rate and quarantine-cause figures by
 * project id alone, at /projects/:project/stats: the console counterpart to
 * `factoryd override-rate`, per project. Strictly observability.
 */
export function ProjectStatsScreen() {
  const { project = "" } = useParams();
  const load = useLoadProject(project, projectStatsPath, () => undefined);
  return (
    <>
      <PageHeader
        title={project === "" ? "Project stats" : project}
        {...(project === "" ? {} : { description: "Stats" })}
      />
      <PageBody>
        {project === "" ? (
          <ProjectIdForm initial="" loading={false} onLoad={load} />
        ) : (
          <ProjectStatsPanel key={project} project={project} />
        )}
      </PageBody>
    </>
  );
}
