import { useUpdateRequestSpec } from "@/api/requestQueries";
import type { RequestSummary } from "@/domain/request";

import { AcceptanceCriteriaList } from "./AcceptanceCriteriaList";
import { FileContentSection } from "./FileContentSection";
import type { EditorBinding } from "./useEditSession";
import { foldsContent, specSummary } from "./requestDetailLogic";

export interface SpecSectionProps {
  readonly request: RequestSummary;
  readonly editable: boolean;
  readonly editing: boolean;
  readonly onStartEdit: () => void;
  readonly onStopEdit: () => void;
  readonly onFetchCurrent: () => Promise<string>;
  readonly session?: EditorBinding;
}

/** spec.md, with the parsed acceptance criteria under it. Editable in spec_review, and whole there; one closed line once the approval is behind it. */
export function SpecSection({
  request,
  editable,
  editing,
  onStartEdit,
  onStopEdit,
  onFetchCurrent,
  session,
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
      {...(session === undefined ? {} : { session })}
      {...(foldsContent(request.state) ? { foldedSummary: specSummary(request.spec) } : {})}
    >
      <AcceptanceCriteriaList spec={request.spec} />
    </FileContentSection>
  );
}
