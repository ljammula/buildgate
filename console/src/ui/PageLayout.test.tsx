import { render, screen } from "@testing-library/react";

import { PageBody, PageHeader, Section } from "@/ui/PageLayout";

test("PageHeader renders the single h1 with description, actions and breadcrumbs", () => {
  render(
    <PageHeader
      title="Runs"
      description="All builds"
      actions={<button type="button">New</button>}
      breadcrumbs={<nav aria-label="Breadcrumb">Home</nav>}
    />,
  );
  expect(screen.getAllByRole("heading", { level: 1 })).toHaveLength(1);
  expect(screen.getByRole("heading", { level: 1, name: "Runs" })).toBeInTheDocument();
  expect(screen.getByText("All builds")).toBeInTheDocument();
  expect(screen.getByRole("button", { name: "New" })).toBeInTheDocument();
  expect(screen.getByRole("navigation", { name: "Breadcrumb" })).toBeInTheDocument();
});

test("Section is a named region with an h2 and its actions", () => {
  render(
    <PageBody>
      <Section title="Evidence" actions={<button type="button">Refresh</button>}>
        content
      </Section>
    </PageBody>,
  );
  expect(screen.getByRole("heading", { level: 2, name: "Evidence" })).toBeInTheDocument();
  expect(screen.getByRole("button", { name: "Refresh" })).toBeInTheDocument();
  expect(screen.getByText("content")).toBeInTheDocument();
});
