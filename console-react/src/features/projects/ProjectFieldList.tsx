import type { ReactNode } from "react";

export interface ProjectField {
  readonly label: string;
  readonly value: ReactNode;
}

/** Label/value rows, selectable text. */
export function ProjectFieldList({ fields }: { readonly fields: readonly ProjectField[] }) {
  return (
    <dl className="grid grid-cols-[12rem_1fr] gap-x-4 gap-y-2 text-sm">
      {fields.map((field) => (
        <div key={field.label} className="contents">
          <dt className="text-fg-muted">{field.label}</dt>
          <dd className="min-w-0 break-words text-fg">{field.value}</dd>
        </div>
      ))}
    </dl>
  );
}
