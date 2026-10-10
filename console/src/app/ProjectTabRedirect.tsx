import { Navigate, useParams } from "react-router";

import { type ProjectTab, projectsPath } from "@/routes/paths";

export interface ProjectTabRedirectProps {
  readonly tab: ProjectTab;
}

/**
 * An old `/projects/:project/<page>` link lands on the Projects screen with
 * that project open on the tab the page used to show.
 */
export function ProjectTabRedirect({ tab }: ProjectTabRedirectProps) {
  const { project = "" } = useParams();
  return <Navigate replace to={projectsPath(project, tab)} />;
}
