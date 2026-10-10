import { render as renderBare, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { ReactElement } from "react";

import { TooltipProvider } from "@/ui/Tooltip";
import { CopyButton } from "@/ui/CopyButton";

// IconButton's tooltip needs the provider the app mounts once.
const render = (ui: ReactElement) => renderBare(ui, { wrapper: TooltipProvider });

describe.each(["sm", "md"] as const)("CopyButton size %s", (size) => {
  test("copies the text and names itself Copied afterwards", async () => {
    const writeText = vi.fn(() => Promise.resolve());
    render(
      <CopyButton size={size} text="make verify" label="Copy command" writeText={writeText} />,
    );
    await userEvent.click(screen.getByRole("button", { name: "Copy command" }));
    expect(writeText).toHaveBeenCalledWith("make verify");
    expect(await screen.findByRole("button", { name: "Copied" })).toBeInTheDocument();
  });

  test("a refused write keeps the idle name", async () => {
    const writeText = vi.fn(() => Promise.reject(new Error("denied")));
    render(<CopyButton size={size} text="x" label="Copy id" writeText={writeText} />);
    await userEvent.click(screen.getByRole("button", { name: "Copy id" }));
    expect(writeText).toHaveBeenCalled();
    expect(screen.getByRole("button", { name: "Copy id" })).toBeInTheDocument();
  });
});

test("no button is drawn for text that would not paste as it reads", () => {
  const { container } = render(
    <TooltipProvider>
      <CopyButton text={"factoryd retry req-1"} label="Copy command" />
      <CopyButton size="sm" text={"a\nrm -rf x"} label="Copy other" />
    </TooltipProvider>,
  );
  expect(container.querySelector("button")).toBeNull();
});
