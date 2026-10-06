import { useParams } from "react-router";

import { RunPage } from "@/features/run-detail/RunPage";

/** The run page at `/runs/:id`. Keyed by id so every piece of per-run state starts fresh for another run. */
export function RunDetailScreen() {
  const { id = "" } = useParams();
  return <RunPage key={id} id={id} />;
}
