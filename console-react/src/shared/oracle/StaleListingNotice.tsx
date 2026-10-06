import { ErrorCallout } from "@/ui/ErrorDisplay";

export interface StaleListingNoticeProps {
  /** Why the reload failed. */
  readonly error: unknown;
}

/** The reload-failed notice shown above a stale listing. */
export function StaleListingNotice({ error }: StaleListingNoticeProps) {
  return (
    <div data-testid="oracle-stale-listing" className="flex flex-col gap-1">
      <ErrorCallout error={error} />
      <p className="text-sm">
        The files below are the last listing that loaded and may be out of date -- Approve is
        disabled until Reload files succeeds.
      </p>
    </div>
  );
}
