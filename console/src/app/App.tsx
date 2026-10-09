import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { useState } from "react";
import { BrowserRouter, Route, Routes } from "react-router";

import { ApiProvider } from "@/api/ApiProvider";
import type { Http } from "@/api/http";
import { defaultQueryStaleTimeMs } from "@/api/polling";
import type { ConsoleConfig } from "@/domain/ops";
import { BoardScreen } from "@/features/board/BoardScreen";
import { NewRequestScreen } from "@/features/new-request/NewRequestScreen";
import { NewRunScreen } from "@/features/new-run/NewRunScreen";
import { OpsScreen } from "@/features/ops/OpsScreen";
import { ProjectListScreen } from "@/features/projects/ProjectListScreen";
import { ProjectReleaseScreen } from "@/features/projects/ProjectReleaseScreen";
import { ProjectObservationsScreen } from "@/features/projects/ProjectObservationsScreen";
import { ProjectStatsScreen } from "@/features/projects/ProjectStatsScreen";
import { RequestDetailScreen } from "@/features/request-detail/RequestDetailScreen";
import { RunDetailScreen } from "@/features/run-detail/RunDetailScreen";
import { RunListScreen } from "@/features/runs/RunListScreen";
import { TriageScreen } from "@/features/triage/TriageScreen";
import { routePatterns } from "@/routes/paths";
import { AppShell } from "@/shared/shell/AppShell";
import { TooltipProvider } from "@/ui/Tooltip";

export interface AppProps {
  readonly http: Http;
  readonly config: ConsoleConfig;
}

function newQueryClient(): QueryClient {
  return new QueryClient({
    defaultOptions: {
      queries: {
        // A read that failed is shown with its error and a Retry; it is not
        // retried silently behind a spinner.
        retry: false,
        staleTime: defaultQueryStaleTimeMs,
        refetchOnWindowFocus: true,
      },
    },
  });
}

/** The route table: one line per screen, every path from routes/paths. */
function AppRoutes() {
  return (
    <Routes>
      <Route path={routePatterns.board} element={<BoardScreen />} />
      <Route path={routePatterns.triage} element={<TriageScreen />} />
      <Route path={routePatterns.newRequest} element={<NewRequestScreen />} />
      <Route path={routePatterns.requestDetail} element={<RequestDetailScreen />} />
      <Route path={routePatterns.runs} element={<RunListScreen />} />
      <Route path={routePatterns.runDetail} element={<RunDetailScreen />} />
      <Route path={routePatterns.newRun} element={<NewRunScreen />} />
      <Route path={routePatterns.projects} element={<ProjectListScreen />} />
      <Route path={routePatterns.projectStats} element={<ProjectStatsScreen />} />
      <Route path={routePatterns.projectObservations} element={<ProjectObservationsScreen />} />
      <Route path={routePatterns.projectRelease} element={<ProjectReleaseScreen />} />
      <Route path={routePatterns.ops} element={<OpsScreen />} />
      {/* An unknown path is the board, as the server serves the console for it. */}
      <Route path="*" element={<BoardScreen />} />
    </Routes>
  );
}

export function App({ http, config }: AppProps) {
  const [queryClient] = useState(newQueryClient);
  return (
    <QueryClientProvider client={queryClient}>
      <ApiProvider http={http} config={config}>
        <TooltipProvider>
          <BrowserRouter>
            <AppShell>
              <AppRoutes />
            </AppShell>
          </BrowserRouter>
        </TooltipProvider>
      </ApiProvider>
    </QueryClientProvider>
  );
}
