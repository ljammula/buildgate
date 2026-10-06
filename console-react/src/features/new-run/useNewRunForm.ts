import { useState } from "react";

export type NewRunField = "ticket" | "workspace" | "spec" | "repository" | "temporalAddress";

export const newRunFieldLabels: Readonly<Record<NewRunField, string>> = {
  ticket: "Ticket",
  workspace: "Workspace path",
  spec: "Spec path",
  repository: "Repository",
  temporalAddress: "Temporal address",
};

export type NewRunValues = Readonly<Record<NewRunField, string>>;

const fields = Object.keys(newRunFieldLabels) as NewRunField[];

/**
 * The intake form's five required fields. Errors appear once a start has been
 * attempted and clear as the field is filled; starting never posts an
 * invalid form.
 */
export function useNewRunForm(initial: Partial<NewRunValues>) {
  const [values, setValues] = useState<NewRunValues>({
    ticket: "",
    workspace: "",
    spec: "",
    repository: "",
    temporalAddress: "",
    ...initial,
  });
  const [attempted, setAttempted] = useState(false);
  const missing = (name: NewRunField) => values[name].trim() === "";
  return {
    values,
    set: (name: NewRunField, value: string) => {
      setValues((current) => ({ ...current, [name]: value }));
    },
    /** "<Label> is required" for an empty field, once a start was attempted. */
    errorFor: (name: NewRunField): string | undefined =>
      attempted && missing(name) ? `${newRunFieldLabels[name]} is required` : undefined,
    /** Marks the attempt and reports whether every field is filled. */
    validate: (): boolean => {
      setAttempted(true);
      return !fields.some(missing);
    },
  };
}
