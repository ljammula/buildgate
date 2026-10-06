import { useNavigate } from "react-router";

/**
 * What the Load button does: the page already showing `current` refreshes in
 * place (keeping its data if the refresh fails); another id navigates to that
 * project's page, which starts from nothing, so a failed lookup for project B
 * can never leave project A's figures next to B's error.
 */
export function useLoadProject(
  current: string,
  pathFor: (project: string) => string,
  refresh: () => void,
): (project: string) => void {
  const navigate = useNavigate();
  return (project) => {
    if (project === current) refresh();
    else void navigate(pathFor(project));
  };
}
