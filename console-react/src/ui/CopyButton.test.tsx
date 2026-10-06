import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

import { CopyButton } from "@/ui/CopyButton";

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
