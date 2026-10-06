export interface DiffFile {
  readonly name: string;
  /** The index of the diff line the file's section starts at. */
  readonly line: number;
}

const gitHeader = /^diff --git a\/(.*) b\/(.*)$/;
const plusHeader = /^\+\+\+ b\/(.*)$/;

/**
 * Each changed file's name and the diff line its section starts at, in
 * first-seen order: `diff --git` headers preferred (present for a multi-file
 * git diff and unambiguous even for a rename), falling back to `+++ b/X`
 * headers when there are none (a single-file compare).
 */
export function changedFileLines(lines: readonly string[]): readonly DiffFile[] {
  const collect = (pattern: RegExp, group: number): DiffFile[] => {
    const files: DiffFile[] = [];
    lines.forEach((text, line) => {
      const name = pattern.exec(text)?.[group];
      if (name !== undefined) files.push({ name, line });
    });
    return files;
  };
  const fromGit = collect(gitHeader, 2);
  return fromGit.length > 0 ? fromGit : collect(plusHeader, 1);
}
