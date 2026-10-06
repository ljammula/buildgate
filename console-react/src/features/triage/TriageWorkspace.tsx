import { useEffect, useState } from "react";

import { useApi } from "@/api/ApiProvider";
import { useRequest } from "@/api/requestQueries";
import { compareTimestamps } from "@/domain/elapsed";
import type { RequestSummary } from "@/domain/request";
import { ApproveDialog } from "@/shared/approval/ApproveDialog";
import { RejectDialog } from "@/shared/approval/RejectDialog";
import { useNow } from "@/ui/Time";

import { TriageDetail } from "./TriageDetail";
import { TriageList } from "./TriageList";
import { TriageOtherDetail } from "./TriageOtherDetail";
import { type NonEmptyRequests, decidesInPlace } from "./triageModel";
import { useTriageKeys } from "./useTriageKeys";

export interface TriageWorkspaceProps {
  readonly requests: NonEmptyRequests;
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
export function TriageWorkspace({ requests }: TriageWorkspaceProps) {
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
  readonly canWrite: boolean;
  readonly now: Date;
  readonly deciding: Deciding | null;
  readonly onDeciding: (deciding: Deciding | null) => void;
  readonly onFocus: (index: number) => void;
}

// Keyed by the focused id, so each request gets its own detail query state.
function TriageFocused(props: FocusedProps) {
  return decidesInPlace(props.focused) ? (
    <TriageDecidable {...props} />
  ) : (
    <TriageOther {...props} />
  );
}

// A request that needs you but is decided on its own page: no detail fetch, no a/r.
function TriageOther({ requests, focused, focusedIndex, now, onFocus }: FocusedProps) {
  useTriageKeys(
    {
      move: (delta) => {
        onFocus(focusedIndex + delta);
      },
      approve: () => undefined,
      reject: () => undefined,
    },
    false,
  );
  return (
    <div className="grid gap-4 lg:grid-cols-[22rem_minmax(0,1fr)]">
      <TriageList requests={requests} focusedId={focused.id} onFocus={onFocus} />
      <TriageOtherDetail request={focused} now={now} />
    </div>
  );
}

function TriageDecidable({
  requests,
  focused,
  focusedIndex,
  canWrite,
  now,
  deciding,
  onDeciding,
  onFocus,
}: FocusedProps) {
  const detailQuery = useRequest(focused.id);
  const detail = detailQuery.data ?? null;
  const { refetch } = detailQuery;

  // The board record is newer than the detail shown: fetch it again. Found in
  // review, a spec edited while the request stayed in the same review state
  // kept serving the old text, so an operator could approve content they
  // never saw. (The Refresh button invalidates from its own click handler.)
  const detailStale = detail !== null && compareTimestamps(focused.updatedAt, detail.updatedAt) > 0;
  useEffect(() => {
    if (detailStale) void refetch();
  }, [detailStale, refetch]);

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
