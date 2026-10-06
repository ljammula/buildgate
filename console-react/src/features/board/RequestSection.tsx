import { sectionLabels, type RequestBoardSection } from "@/domain/boardFilters";
import type { RequestSummary } from "@/domain/request";
import { Section } from "@/ui/PageLayout";
import { Table, TableBody, TableFrame, TableHead, TableHeaderCell, TableRow } from "@/ui/Table";

import { RequestRow } from "./RequestRow";

export interface RequestSectionProps {
  readonly section: RequestBoardSection;
  readonly requests: readonly RequestSummary[];
  readonly now: Date;
  /** More than one project on the board: only then does a row's project say anything. */
  readonly showProject: boolean;
}

/** One board section ("Needs you (2)"): a compact table of its requests. */
export function RequestSection({ section, requests, now, showProject }: RequestSectionProps) {
  const label = sectionLabels[section];
  return (
    <Section title={`${label} (${requests.length})`}>
      <TableFrame>
        <Table
          aria-label={label}
          className={showProject ? "min-w-[56rem] table-fixed" : "min-w-[48rem] table-fixed"}
        >
          <TableHead>
            <TableRow className="hover:bg-transparent">
              <TableHeaderCell className="w-[22rem]">Status</TableHeaderCell>
              <TableHeaderCell>Request</TableHeaderCell>
              {showProject ? <TableHeaderCell className="w-24">Project</TableHeaderCell> : null}
              <TableHeaderCell className="w-60">Progress</TableHeaderCell>
              <TableHeaderCell className="w-32">Usage</TableHeaderCell>
              <TableHeaderCell className="w-28">Updated</TableHeaderCell>
            </TableRow>
          </TableHead>
          <TableBody>
            {requests.map((request) => (
              <RequestRow key={request.id} request={request} now={now} showProject={showProject} />
            ))}
          </TableBody>
        </Table>
      </TableFrame>
    </Section>
  );
}
