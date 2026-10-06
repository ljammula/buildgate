import { useUpdateRequestSpec } from "@/api/requestQueries";
import type { RequestSummary } from "@/domain/request";

import { AcceptanceCriteriaList } from "./AcceptanceCriteriaList";
import { FileContentSection } from "./FileContentSection";

export interface SpecSectionProps {
  readonly request: RequestSummary;
  readonly editable: boolean;
  readonly editing: boolean;
  readonly onStartEdit: () => void;
  readonly onStopEdit: () => void;
  readonly onFetchCurrent: () => Promise<string>;
}

/** spec.md, with the parsed acceptance criteria under it. Editable in spec_review. */
export function SpecSection({
  request,
  editable,
  editing,
  onStartEdit,
  onStopEdit,
  onFetchCurrent,
}: SpecSectionProps) {
  const update = useUpdateRequestSpec(request.id);
  return (
    <FileContentSection
      title="Spec"
      path="spec.md"
      fullPath={request.specFullPath}
      content={request.spec}
      editable={editable}
      editing={editing}
      onStartEdit={onStartEdit}
      onStopEdit={onStopEdit}
      onSave={async (content, baseSha256) => {
        await update.mutateAsync({ content, baseSha256 });
      }}
      onFetchCurrent={onFetchCurrent}
    >
      <AcceptanceCriteriaList spec={request.spec} />
    </FileContentSection>
  );
}
