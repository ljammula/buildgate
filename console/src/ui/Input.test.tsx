import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

import { Checkbox, Field, FormField, Input, Select, Textarea } from "@/ui/Input";

test("Field wires label, hint and error to the control", () => {
  render(
    <Field label="Title" hint="Short and specific" error="Required">
      <Input />
    </Field>,
  );
  const input = screen.getByLabelText("Title");
  expect(input).toHaveAttribute("aria-invalid", "true");
  expect(input).toHaveAccessibleDescription("Short and specific Required");
});

test("Field without an error is not marked invalid", () => {
  render(
    <Field label="Title">
      <Input />
    </Field>,
  );
  const input = screen.getByLabelText("Title");
  expect(input).not.toHaveAttribute("aria-invalid");
  expect(input).not.toHaveAttribute("aria-describedby");
});

test("controls accept typing, selection and toggling", async () => {
  render(
    <>
      <Field label="Spec">
        <Textarea mono />
      </Field>
      <Field label="Mode">
        <Select>
          <option value="a">A</option>
          <option value="b">B</option>
        </Select>
      </Field>
      <Field label="Auto">
        <Checkbox />
      </Field>
    </>,
  );
  await userEvent.type(screen.getByRole("textbox", { name: "Spec" }), "hello");
  expect(screen.getByRole("textbox", { name: "Spec" })).toHaveValue("hello");
  await userEvent.selectOptions(screen.getByRole("combobox", { name: "Mode" }), "b");
  expect(screen.getByRole("combobox", { name: "Mode" })).toHaveValue("b");
  await userEvent.click(screen.getByRole("checkbox", { name: "Auto" }));
  expect(screen.getByRole("checkbox", { name: "Auto" })).toBeChecked();
});

test("FormField is the same component as Field", () => {
  expect(FormField).toBe(Field);
});
