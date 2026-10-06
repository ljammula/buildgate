import type { RequestSummary } from "@/domain/request";

/** The props every approval flow component shares. */
export interface FlowProps {
  /**
   * The request exactly as the operator is looking at it. Anything the flow
   * sends or states (the content hashes of an approval, the state
   * transition, the resume step) is derived from this object, never from a
   * fresh fetch at confirm time.
   */
  readonly request: RequestSummary;
  /** Whether the flow is shown. Closed (or `canWrite` false) renders nothing. */
  readonly open: boolean;
  /** Called with `false` when the operator cancels or the write succeeds. */
  readonly onOpenChange: (open: boolean) => void;
  /** Called once with the server's new record after the write succeeds, before the flow closes. */
  readonly onDone?: (request: RequestSummary) => void;
}
