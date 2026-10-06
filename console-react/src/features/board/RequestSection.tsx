import { sectionLabels, type RequestBoardSection } from "@/domain/boardFilters";
import type { RequestSummary } from "@/domain/request";
import { Section } from "@/ui/PageLayout";
import { Table, TableBody, TableHead, TableHeaderCell, TableRow } from "@/ui/Table";

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
      <div className="border-border overflow-x-auto rounded-lg border">
        <Table aria-label={label}>
          <TableHead>
            <TableRow className="hover:bg-transparent">
              <TableHeaderCell>Status</TableHeaderCell>
              <TableHeaderCell>Request</TableHeaderCell>
              <TableHeaderCell>Project</TableHeaderCell>
              <TableHeaderCell>Progress</TableHeaderCell>
              <TableHeaderCell>Usage</TableHeaderCell>
              <TableHeaderCell>Updated</TableHeaderCell>
            </TableRow>
          </TableHead>
          <TableBody>
            {requests.map((request) => (
              <RequestRow key={request.id} request={request} now={now} />
            ))}
          </TableBody>
        </Table>
      </div>
    </Section>
  );
}
