import { render, screen } from "@testing-library/react";

import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeaderCell,
  TableRow,
} from "@/ui/Table";

function Example() {
  return (
    <Table aria-label="Runs">
      <TableHead>
        <TableRow>
          <TableHeaderCell>Run</TableHeaderCell>
          <TableHeaderCell>State</TableHeaderCell>
        </TableRow>
      </TableHead>
      <TableBody>
        <TableRow selected>
          <TableCell>run-1</TableCell>
          <TableCell>done</TableCell>
        </TableRow>
      </TableBody>
    </Table>
  );
}

test("renders a real table with column headers and cells", () => {
  render(<Example />);
  expect(screen.getByRole("table", { name: "Runs" })).toBeInTheDocument();
  expect(screen.getAllByRole("columnheader").map((h) => h.textContent)).toEqual(["Run", "State"]);
  expect(screen.getByRole("cell", { name: "run-1" })).toBeInTheDocument();
});

test("a selected row is exposed as selected", () => {
  render(<Example />);
  const rows = screen.getAllByRole("row");
  expect(rows[1]).toHaveAttribute("aria-selected", "true");
  expect(rows[0]).not.toHaveAttribute("aria-selected");
});
