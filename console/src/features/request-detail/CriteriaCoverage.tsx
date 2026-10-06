import { Check } from "lucide-react";

import { planCoverage } from "@/domain/planCoverage";
import type { RequestSummary } from "@/domain/request";
import {
  Table,
  TableBody,
  TableCell,
  TableFrame,
  TableHead,
  TableHeaderCell,
  TableRow,
} from "@/ui/Table";
import { cn } from "@/ui/cn";

import { Panel } from "./Panel";

function list(numbers: readonly number[]): string {
  return numbers.join(", ");
}

/**
 * Plan review's traceability view: every acceptance criterion of the approved
 * spec, in full, against the tickets that claim it. The tickets name criteria
 * by number only and the spec is not on the page at this state, so without
 * this a reviewer reads "- 2" with nothing to check it against. A view over
 * the shown files, not a document: the approval still covers the ticket
 * files below, whole. Absent when the spec has no numbered criteria or no
 * ticket has content.
 */
export function CriteriaCoverage({ request }: { readonly request: RequestSummary }) {
  const tickets = request.tickets.filter((t) => t.content !== "");
  const coverage = planCoverage(request.spec, tickets);
  if (tickets.length === 0 || coverage.criteria.length === 0) return null;
  const total = coverage.criteria.length;
  const missing = coverage.unclaimed.length;
  return (
    <Panel title="Criteria coverage" testId="criteria-coverage">
      <p role="status" className={cn("text-sm", missing > 0 && "text-tone-danger")}>
        {missing === 0
          ? total === 1
            ? "The acceptance criterion is claimed by a ticket."
            : `Each of the ${total} acceptance criteria is claimed by a ticket.`
          : `Claimed by no ticket: ${missing === 1 ? "criterion" : "criteria"} ${list(coverage.unclaimed)}.`}
      </p>
      <TableFrame>
        <Table>
          <TableHead>
            <TableRow>
              <TableHeaderCell>Acceptance criterion</TableHeaderCell>
              {tickets.map((ticket) => (
                <TableHeaderCell key={ticket.index} className="w-10 text-center">
                  <span aria-hidden="true">{`T${ticket.index}`}</span>
                  <span className="sr-only">{`Ticket ${ticket.index}`}</span>
                </TableHeaderCell>
              ))}
            </TableRow>
          </TableHead>
          <TableBody>
            {coverage.criteria.map((criterion) => (
              <TableRow
                key={criterion.number}
                data-unclaimed={criterion.tickets.length === 0}
                className={cn("h-auto", criterion.tickets.length === 0 && "bg-tone-danger-soft")}
              >
                <TableHeaderCell
                  scope="row"
                  className="text-fg h-auto py-1.5 text-sm font-normal whitespace-normal"
                >
                  {criterion.text}
                </TableHeaderCell>
                {tickets.map((ticket) => (
                  <TableCell key={ticket.index} className="text-center">
                    {criterion.tickets.includes(ticket.index) ? (
                      <>
                        <Check aria-hidden="true" className="text-tone-success inline size-4" />
                        <span className="sr-only">covers</span>
                      </>
                    ) : null}
                  </TableCell>
                ))}
              </TableRow>
            ))}
          </TableBody>
        </Table>
      </TableFrame>
      {coverage.stray.map((stray) => (
        <p key={stray.ticket} className="text-tone-warning text-sm">
          {`Ticket ${stray.ticket} claims ${list(stray.numbers)}: the spec has ${total} ${
            total === 1 ? "criterion" : "criteria"
          }.`}
        </p>
      ))}
      {coverage.unreadable.length === 0 ? null : (
        <p className="text-tone-warning text-sm">
          {`The covered-criteria list of ticket ${list(coverage.unreadable)} does not parse, so it claims nothing.`}
        </p>
      )}
    </Panel>
  );
}
