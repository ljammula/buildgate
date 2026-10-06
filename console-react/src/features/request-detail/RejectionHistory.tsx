import type { Rejection } from "@/domain/request";
import { EscapedText } from "@/shared/oracle/EscapedText";
import { Disclosure } from "@/ui/Disclosure";

import { rejectionHeading } from "./requestDetailLogic";

/** Every structured rejection or send-back the request recorded. Display only: `fromState` routes nothing. */
export function RejectionHistory({ rejections }: { readonly rejections: readonly Rejection[] }) {
  return (
    <Disclosure
      bare
      headingLevel={null}
      title={`Rejection history (${rejections.length})`}
      testId="rejection-history"
    >
      <ul className="flex flex-col gap-3">
        {rejections.map((rejection, i) => (
          <li key={i} className="text-sm">
            <p className="text-fg-muted">{rejectionHeading(rejection)}</p>
            <EscapedText text={rejection.reason} />
          </li>
        ))}
      </ul>
    </Disclosure>
  );
}
