// A command's arguments as one line a shell reads back as the same arguments.

// `=` and `%` are left out: zsh expands a leading `=word` and reads `%1` as a
// job, and `NAME=value` first on a line is an assignment.
const plain = /^[A-Za-z0-9_@+:,./-]+$/;

/** One argument, single-quoted unless every character is safe unquoted. */
export function shellQuote(argument: string): string {
  if (plain.test(argument)) return argument;
  return `'${argument.replaceAll("'", "'\\''")}'`;
}

/** The arguments joined so that pasting the line runs the same command. */
export function shellJoin(args: readonly string[]): string {
  return args.map(shellQuote).join(" ");
}
