import { EscapedText } from "@/shared/oracle/EscapedText";
import { Disclosure } from "@/ui/Disclosure";

export interface ReviewMaskedPathsProps {
  /** `Attempt.reviewMaskedPaths`: paths of the repository, so shown as text. */
  readonly paths: readonly string[];
}

/** More paths than this are folded behind one line. */
const INLINE_PATHS = 4;

const LEAD = "This review read these files as they were before the build";

/**
 * The instruction files a review attempt read from the base commit rather
 * than from the build's worktree, so the operator can see a build could not
 * reword what its reviewer was told. Nothing for an attempt that masked none.
 */
export function ReviewMaskedPaths({ paths }: ReviewMaskedPathsProps) {
  if (paths.length === 0) return null;
  const list = (
    <ul className="flex flex-col font-mono text-xs text-fg-muted">
      {paths.map((path, i) => (
        // The server's list is ordered and may end in an "... and N more" line.
        <li key={i}>
          <EscapedText text={path} />
        </li>
      ))}
    </ul>
  );
  if (paths.length <= INLINE_PATHS) {
    return (
      <div data-testid="review-masked-paths">
        <p>{LEAD}:</p>
        {list}
      </div>
    );
  }
  return (
    <Disclosure
      bare
      headingLevel={null}
      title={LEAD}
      summary={`${paths.length} listed`}
      testId="review-masked-paths"
    >
      {list}
    </Disclosure>
  );
}
