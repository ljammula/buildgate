import { sectionLabels, type RequestBoardSection } from "@/domain/boardFilters";
import type { RequestSummary } from "@/domain/request";
import { Section } from "@/ui/PageLayout";
import { Table, TableBody, TableFrame, TableHead, TableHeaderCell, TableRow } from "@/ui/Table";

import { RequestRow } from "./RequestRow";

export interface RequestSectionProps {
  readonly section: RequestBoardSection;
  readonly requests: readonly RequestSummary[];
  readonly now: Date;
}

/** One board section ("Needs you (2)"): a compact table of its requests. */
export function RequestSection({ section, requests, now }: RequestSectionProps) {
  const label = sectionLabels[section];
  return (
    <Section title={`${label} (${requests.length})`}>
      <TableFrame>
        <Table aria-label={label} className="min-w-[56rem] table-fixed">
          <TableHead>
            <TableRow className="hover:bg-transparent">
              <TableHeaderCell className="w-56">Status</TableHeaderCell>
              <TableHeaderCell>Request</TableHeaderCell>
              <TableHeaderCell className="w-24">Project</TableHeaderCell>
              <TableHeaderCell className="w-60">Progress</TableHeaderCell>
              <TableHeaderCell className="w-48">Usage</TableHeaderCell>
              <TableHeaderCell className="w-44">Updated</TableHeaderCell>
            </TableRow>
          </TableHead>
          <TableBody>
            {requests.map((request) => (
              <RequestRow key={request.id} request={request} now={now} />
            ))}
          </TableBody>
        </Table>
      </TableFrame>
    </Section>
  );
}
