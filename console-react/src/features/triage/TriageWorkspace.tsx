import { useEffect, useState } from "react";

import { useApi } from "@/api/ApiProvider";
import { useRequest } from "@/api/requestQueries";
import type { RequestSummary } from "@/domain/request";
import { ApproveDialog } from "@/shared/approval/ApproveDialog";
import { RejectDialog } from "@/shared/approval/RejectDialog";
import { useNow } from "@/ui/Time";

import { TriageDetail } from "./TriageDetail";
import { TriageList } from "./TriageList";
import { type NonEmptyRequests } from "./triageModel";
import { useTriageKeys } from "./useTriageKeys";

export interface TriageWorkspaceProps {
  readonly requests: NonEmptyRequests;
  /** Bumped by the Refresh button: the focused detail is fetched again. */
  readonly refreshes: number;
}

interface Focus {
  readonly id: string;
  readonly index: number;
}

interface Deciding {
  readonly kind: "approve" | "reject";
  /** The record exactly as displayed when the key was pressed: what an approval hashes. */
  readonly record: RequestSummary;
}

/**
 * The list, the focused request's detail and the decision dialogs. Focus
 * follows the request's id; when it leaves the list (just approved) it falls
 * to the same position, clamped.
 */
export function TriageWorkspace({ requests, refreshes }: TriageWorkspaceProps) {
  const { canWrite } = useApi();
  const now = useNow(30_000);
  const [focus, setFocus] = useState<Focus>({ id: requests[0].id, index: 0 });
  const [deciding, setDeciding] = useState<Deciding | null>(null);

  const byId = requests.findIndex((r) => r.id === focus.id);
  const focusedIndex = byId >= 0 ? byId : Math.min(focus.index, requests.length - 1);
  const focused = requests[focusedIndex] ?? requests[0];

  return (
    <TriageFocused
      key={focused.id}
      requests={requests}
      focused={focused}
      focusedIndex={focusedIndex}
      refreshes={refreshes}
      canWrite={canWrite}
      now={now}
      deciding={deciding}
      onDeciding={setDeciding}
      onFocus={(index) => {
        const next = requests[Math.min(Math.max(index, 0), requests.length - 1)];
        if (next !== undefined) setFocus({ id: next.id, index });
      }}
    />
  );
}

interface FocusedProps {
  readonly requests: NonEmptyRequests;
  readonly focused: RequestSummary;
  readonly focusedIndex: number;
  readonly refreshes: number;
  readonly canWrite: boolean;
  readonly now: Date;
  readonly deciding: Deciding | null;
  readonly onDeciding: (deciding: Deciding | null) => void;
  readonly onFocus: (index: number) => void;
}

// Keyed by the focused id, so each request gets its own detail query state.
function TriageFocused({
  requests,
  focused,
  focusedIndex,
  refreshes,
  canWrite,
  now,
  deciding,
  onDeciding,
  onFocus,
}: FocusedProps) {
  const detailQuery = useRequest(focused.id);
  const detail = detailQuery.data ?? null;
  const { refetch, isFetching } = detailQuery;

  // Fetch the content again when the operator hits Refresh, and when the
  // board record is newer than the one shown: found in review, a spec edited
  // while the request stayed in the same review state kept serving the old
  // text, so an operator could approve content they never saw.
  const detailStale = detail !== null && focused.updatedAt > detail.updatedAt;
  useEffect(() => {
    if (refreshes > 0) void refetch();
  }, [refreshes, refetch]);
  useEffect(() => {
    if (detailStale && !isFetching) void refetch();
  }, [detailStale, isFetching, refetch]);

  const decide = (kind: Deciding["kind"]): void => {
    if (!canWrite || detail === null) return;
    onDeciding({ kind, record: detail });
  };
  useTriageKeys(
    {
      move: (delta) => {
        onFocus(focusedIndex + delta);
      },
      approve: () => {
        decide("approve");
      },
      reject: () => {
        decide("reject");
      },
    },
    deciding !== null,
  );

  const close = (open: boolean): void => {
    if (!open) onDeciding(null);
  };
  return (
    <div className="grid gap-4 lg:grid-cols-[22rem_minmax(0,1fr)]">
      <TriageList requests={requests} focusedId={focused.id} onFocus={onFocus} />
      <TriageDetail
        request={focused}
        detail={detail}
        detailError={detail === null ? detailQuery.error : null}
        canWrite={canWrite}
        acting={deciding !== null}
        now={now}
        onApprove={() => {
          decide("approve");
        }}
        onReject={() => {
          decide("reject");
        }}
      />
      {deciding === null ? null : deciding.kind === "approve" ? (
        <ApproveDialog
          open
          request={deciding.record}
          costSummary={focused.costSummary}
          onOpenChange={close}
        />
      ) : (
        <RejectDialog open request={deciding.record} onOpenChange={close} />
      )}
    </div>
  );
}
