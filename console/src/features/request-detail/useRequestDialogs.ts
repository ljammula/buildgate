import { useRef, useState } from "react";

import { expectedSha256For } from "@/domain/contentHash";
import type { RequestSummary } from "@/domain/request";
import type { TicketOracleShown } from "@/shared/oracle/TicketOraclePanel";

export type DialogKind =
  "approve" | "reject" | "retry" | "cancel" | "sendBack" | "resumeRound" | "resumeScratch";

interface ActiveDialog {
  readonly kind: DialogKind;
  /**
   * The request exactly as the page showed it when the operator pressed the
   * button. Request changes and Send back work from this record and name its
   * stage, whatever arrives while the name prompt or the dialog is up.
   */
  readonly record: RequestSummary;
  /** The approval's `expected_sha256` override; null derives it from the shown request. */
  readonly expected: Record<string, string> | null;
  /** Resolves the oracle panel's pending approve once the dialog is gone. */
  readonly settle: (() => void) | null;
}

export interface RequestDialogs {
  readonly active: ActiveDialog | null;
  /** A dialog is open: every other action waits. */
  readonly acting: boolean;
  readonly open: (kind: Exclude<DialogKind, "approve">) => void;
  /** spec_review / plan_review approval; at plan_review it waits for the ticket oracle files to have been shown. */
  readonly openApprove: () => void;
  /** oracle_review approval for the oracle files the panel displayed; settles when the flow ends. */
  readonly approveOracle: (expectedSha256: Record<string, string>) => Promise<void>;
  readonly onOpenChange: (open: boolean) => void;
  readonly onDone: () => void;
  /** What the plan_review ticket-oracle panel has displayed; null until it first reports. */
  readonly ticketOracle: TicketOracleShown | null;
  readonly onTicketOracleChanged: (shown: TicketOracleShown) => void;
  /** Changes whenever the panel must start over, which remounts it. */
  readonly ticketOracleNonce: number;
}

/** Whether a plan_review approval must wait for the ticket-oracle panel. */
export function awaitsTicketOracle(request: RequestSummary): boolean {
  return request.state === "plan_review" && request.tickets.length > 0;
}

/**
 * Which approval flow dialog is open, and the plan_review ticket-oracle
 * hand-off. Plan approval pins every ticket's materialized oracle files, so
 * it sends the ticket-spec hashes MERGED with the hashes of the oracle files
 * the panel displayed, and is not offered until the panel reports all of them
 * shown. A plan approval that did not complete (refused, or backed out of)
 * restarts the panel: whatever was refused (a file changed since it was
 * shown) must be listed and opened again.
 */
export function useRequestDialogs(request: RequestSummary): RequestDialogs {
  const [active, setActive] = useState<ActiveDialog | null>(null);
  const [ticketOracle, setTicketOracle] = useState<TicketOracleShown | null>(null);
  const [nonce, setNonce] = useState(0);
  const finished = useRef(false);

  function openApprove() {
    finished.current = false;
    if (awaitsTicketOracle(request)) {
      if (ticketOracle === null || !ticketOracle.complete) return;
      setActive({
        kind: "approve",
        record: request,
        expected: { ...expectedSha256For(request), ...ticketOracle.hashes },
        settle: null,
      });
      return;
    }
    setActive({ kind: "approve", record: request, expected: null, settle: null });
  }

  return {
    active,
    acting: active !== null,
    open: (kind) => {
      finished.current = false;
      setActive({ kind, record: request, expected: null, settle: null });
    },
    openApprove,
    approveOracle: (expectedSha256) => {
      finished.current = false;
      return new Promise<void>((resolve) => {
        setActive({
          kind: "approve",
          record: request,
          expected: expectedSha256,
          settle: () => {
            resolve();
          },
        });
      });
    },
    onOpenChange: (open) => {
      if (open) return;
      active?.settle?.();
      if (active?.kind === "approve" && !finished.current && awaitsTicketOracle(request)) {
        setTicketOracle(null);
        setNonce((n) => n + 1);
      }
      setActive(null);
    },
    onDone: () => {
      finished.current = true;
    },
    ticketOracle,
    onTicketOracleChanged: setTicketOracle,
    ticketOracleNonce: nonce,
  };
}
