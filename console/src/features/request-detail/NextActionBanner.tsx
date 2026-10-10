import { ArrowRight } from "lucide-react";

import { TextWithCode } from "@/ui/TextWithCode";

/** The single factory-authored line telling the operator what to do next. */
export function NextActionBanner({ text }: { readonly text: string }) {
  return (
    <div
      data-testid="next-action-banner"
      className="bg-accent-soft text-accent flex items-start gap-2 rounded-md px-3 py-2 text-sm font-semibold"
    >
      <ArrowRight aria-hidden="true" className="mt-0.5 size-4 shrink-0" />
      <p className="min-w-0 break-words whitespace-pre-wrap">
        <TextWithCode text={text} />
      </p>
    </div>
  );
}
