import { ChevronDown, ChevronRight } from "lucide-react";
import { useState } from "react";

import type { Rejection } from "@/domain/request";
import { EscapedText } from "@/shared/oracle/EscapedText";

import { rejectionHeading } from "./requestDetailLogic";

/** Every structured rejection or send-back the request recorded. Display only: `fromState` routes nothing. */
export function RejectionHistory({ rejections }: { readonly rejections: readonly Rejection[] }) {
  const [open, setOpen] = useState(false);
  const Chevron = open ? ChevronDown : ChevronRight;
  return (
    <div data-testid="rejection-history" className="flex flex-col gap-2">
      <button
        type="button"
        aria-expanded={open}
        onClick={() => {
          setOpen(!open);
        }}
        className="text-fg flex items-center gap-1 text-left text-sm font-medium"
      >
        <Chevron aria-hidden="true" className="size-4" />
        {`Rejection history (${rejections.length})`}
      </button>
      {open ? (
        <ul className="flex flex-col gap-3">
          {rejections.map((rejection, i) => (
            <li key={i} className="text-sm">
              <p className="text-fg-muted">{rejectionHeading(rejection)}</p>
              <EscapedText text={rejection.reason} />
            </li>
          ))}
        </ul>
      ) : null}
    </div>
  );
}
