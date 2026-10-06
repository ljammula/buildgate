import { useApi } from "@/api/ApiProvider";
import { useResumeRequest } from "@/api/requestQueries";
import { stateLabel } from "@/domain/status";

import { FlowDialog } from "./FlowDialog";
import type { FlowProps } from "./flowTypes";
import { OperatorGate } from "./OperatorGate";

export interface ResumeDialogProps extends FlowProps {
  /** `round` continues the lost step; `scratch` rebuilds the ticket (a fresh, paid run). */
  readonly from: "round" | "scratch";
}

interface ResumeCopy {
  readonly title: string;
  readonly body: string;
  readonly confirm: string;
}

/**
 * The resume_review confirmation texts. A lost step other than `building`
 * (`request.resume.fromState`) is always a rerun of that step, whatever
 * `from` says; only a lost build distinguishes Resume from Rebuild.
 */
export function resumeCopy(request: ResumeDialogProps["request"], from: string): ResumeCopy {
  const step = request.resume?.fromState ?? "";
  const rerun = step !== "" && step !== "building";
  if (rerun) {
    return {
      title: "Rerun this step",
      body: `Run the lost ${stateLabel(step)} step again.`,
      confirm: "Rerun step",
    };
  }
  if (from === "scratch") {
    return {
      title: "Rebuild from scratch",
      body: "Discard the lost build and rebuild the ticket from the start. This is a fresh, paid run.",
      confirm: "Rebuild",
    };
  }
  return {
    title: "Resume this request",
    body: "Continue the lost step where the worker stopped.",
    confirm: "Resume",
  };
}

/**
 * The resume_review confirmation: states what will run, then POSTs
 * `/requests/{id}/resume` with `{ from, by }`. Its dismiss button reads
 * "Back". Renders nothing when closed or when the console cannot write.
 */
export function ResumeDialog({ request, from, open, onOpenChange, onDone }: ResumeDialogProps) {
  const { canWrite } = useApi();
  if (!open || !canWrite) return null;
  return (
    <OperatorGate onOpenChange={onOpenChange}>
      {(by) => (
        <ResumeBody
          request={request}
          from={from}
          by={by}
          onOpenChange={onOpenChange}
          onDone={onDone}
        />
      )}
    </OperatorGate>
  );
}

interface BodyProps {
  readonly request: ResumeDialogProps["request"];
  readonly from: string;
  readonly by: string;
  readonly onOpenChange: (open: boolean) => void;
  readonly onDone: FlowProps["onDone"];
}

function ResumeBody({ request, from, by, onOpenChange, onDone }: BodyProps) {
  const resume = useResumeRequest(request.id);
  const copy = resumeCopy(request, from);
  async function confirm() {
    try {
      const updated = await resume.mutateAsync({ from, by });
      onDone?.(updated);
      onOpenChange(false);
    } catch {
      // The mutation's error is shown by the dialog; stay open.
    }
  }
  return (
    <FlowDialog
      open
      onOpenChange={onOpenChange}
      title={copy.title}
      description={copy.body}
      confirmLabel={copy.confirm}
      cancelLabel="Back"
      pending={resume.isPending}
      error={resume.error}
      onConfirm={() => void confirm()}
    />
  );
}
