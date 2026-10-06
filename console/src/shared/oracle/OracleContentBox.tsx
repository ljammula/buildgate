import type { OracleFileContent } from "@/domain/oracle";
import { CodeBlock } from "@/ui/CodeBlock";

export interface OracleContentBoxProps {
  /** Identifies the box (`data-testid="oracle-content-<keyId>"`). */
  readonly keyId: string;
  readonly content: OracleFileContent;
}

/**
 * The monospace content view of one oracle file (or an "(empty file)"
 * marker). The text is agent-written: CodeBlock renders it as text with
 * hidden characters and invalid bytes (`\xNN`) written out.
 */
export function OracleContentBox({ keyId, content }: OracleContentBoxProps) {
  return (
    <div data-testid={`oracle-content-${keyId}`} className="w-full">
      {content.text === "" ? (
        <p data-testid="oracle-empty-file" className="text-fg-muted text-sm italic">
          (empty file)
        </p>
      ) : (
        <CodeBlock label={`Oracle file ${keyId}`} maxHeight="max-h-96">
          {content.text}
        </CodeBlock>
      )}
    </div>
  );
}
