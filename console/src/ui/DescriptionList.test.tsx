import { render, screen } from "@testing-library/react";

import { DescriptionItem, DescriptionList } from "@/ui/DescriptionList";

describe("DescriptionList", () => {
  test("renders each pair as a dt and its dd inside one dl", () => {
    const { container } = render(
      <DescriptionList>
        <DescriptionItem label="Workspace">/tmp/ws</DescriptionItem>
        <DescriptionItem label="Run">r-1</DescriptionItem>
      </DescriptionList>,
    );
    const list = container.querySelector("dl");
    expect(list).not.toBeNull();
    expect(list?.querySelectorAll(":scope > dt")).toHaveLength(2);
    expect(screen.getByText("Workspace").tagName).toBe("DT");
    expect(screen.getByText("/tmp/ws").tagName).toBe("DD");
  });

  test.each([
    ["sm", "grid-cols-[5.5rem_minmax(0,1fr)]"],
    ["md", "grid-cols-[11rem_minmax(0,1fr)]"],
    ["lg", "grid-cols-[12rem_minmax(0,1fr)]"],
  ] as const)("labelWidth %s sets the label column", (labelWidth, cls) => {
    const { container } = render(
      <DescriptionList labelWidth={labelWidth}>
        <DescriptionItem label="a">b</DescriptionItem>
      </DescriptionList>,
    );
    expect(container.querySelector("dl")).toHaveClass(cls);
  });

  test("defaults to md and merges a className last", () => {
    const { container } = render(
      <DescriptionList className="gap-y-4">
        <DescriptionItem label="a">b</DescriptionItem>
      </DescriptionList>,
    );
    expect(container.querySelector("dl")).toHaveClass("grid-cols-[11rem_minmax(0,1fr)]", "gap-y-4");
    expect(container.querySelector("dl")).not.toHaveClass("gap-y-1.5");
  });

  test("mono makes the value monospace and breakable", () => {
    render(
      <DescriptionList>
        <DescriptionItem label="SHA" mono>
          0e2df58c
        </DescriptionItem>
        <DescriptionItem label="Name">plain</DescriptionItem>
      </DescriptionList>,
    );
    expect(screen.getByText("0e2df58c")).toHaveClass("font-mono", "break-all");
    expect(screen.getByText("plain")).not.toHaveClass("font-mono");
  });

  test("the label may be a node", () => {
    render(
      <DescriptionList>
        <DescriptionItem label={<abbr title="Pull request">PR</abbr>}>#12</DescriptionItem>
      </DescriptionList>,
    );
    expect(screen.getByText("PR").closest("dt")).not.toBeNull();
  });
});
