import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/ui/Tabs";

function Example() {
  return (
    <Tabs defaultValue="spec">
      <TabsList aria-label="Request">
        <TabsTrigger value="spec">Spec</TabsTrigger>
        <TabsTrigger value="plan">Plan</TabsTrigger>
      </TabsList>
      <TabsContent value="spec">Spec body</TabsContent>
      <TabsContent value="plan">Plan body</TabsContent>
    </Tabs>
  );
}

test("shows the default panel", () => {
  render(<Example />);
  expect(screen.getByRole("tab", { name: "Spec" })).toHaveAttribute("aria-selected", "true");
  expect(screen.getByRole("tabpanel", { name: "Spec" })).toHaveTextContent("Spec body");
});

test("arrow keys move to the next tab and its panel", async () => {
  render(<Example />);
  await userEvent.tab();
  expect(screen.getByRole("tab", { name: "Spec" })).toHaveFocus();
  await userEvent.keyboard("{ArrowRight}");
  expect(screen.getByRole("tab", { name: "Plan" })).toHaveAttribute("aria-selected", "true");
  expect(screen.getByRole("tabpanel", { name: "Plan" })).toHaveTextContent("Plan body");
});
