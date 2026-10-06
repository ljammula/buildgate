import { formatLocalTimestamp, relativeAge, tryParseTimestamp } from "@/domain/elapsed";
import { useNow } from "@/ui/Time";

export interface RelativeTimeProps {
  /** RFC3339 instant; an empty or unparseable value renders verbatim. */
  readonly value: string;
  readonly className?: string;
}

/**
 * How long ago `value` was ("5m ago", "3d 4h ago"), with the exact local time
 * on hover and the instant itself in `dateTime`. Ticks once a minute. Use it
 * where the age is the information; keep LocalTimeText where the exact time
 * is (an audit line, a decision history).
 */
export function RelativeTime({ value, className }: RelativeTimeProps) {
  const now = useNow(60_000);
  if (tryParseTimestamp(value) === null) return <span className={className}>{value}</span>;
  return (
    <time dateTime={value} title={formatLocalTimestamp(value)} className={className}>
      {relativeAge(value, now)}
    </time>
  );
}
