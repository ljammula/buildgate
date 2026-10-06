import { type ReactNode } from "react";
import { lexer, type Token, type Tokens } from "marked";

import { escapeInvisible } from "@/domain/textEscape";
import { cn } from "@/ui/cn";

// Minimal Markdown renderer for spec and plan text.
//
// The source is written by a coding agent, so it is untrusted, and this page
// holds the start token. Two rules follow, and a change that breaks either is
// a security hole:
// - Only marked's lexer runs. Its token tree is walked into React elements;
//   marked's HTML-producing entry points are never called, so no HTML string
//   exists anywhere between the source and the DOM.
// - Anything that could make the browser act on agent text is rendered as
//   visible text instead: raw HTML (literal `<script>` characters on screen),
//   images (no outbound request from agent text), and a link whose scheme is
//   not http or https. The Dart console is stricter still and never navigates
//   at all; here an http(s) link is a real anchor, and its target is always
//   written out beside the label so a label cannot hide where it goes.
// Invisible and bidi characters are made visible with escapeInvisible, as for
// any text an operator reads before approving.

/** Nesting past this renders the remaining source as text: bounds the React tree an agent can build. */
const MAX_DEPTH = 24;

export interface MarkdownProps {
  readonly source: string;
  readonly className?: string;
}

const t = escapeInvisible;

/** The normalised absolute URL of an explicit http(s) link, else null. */
function safeHref(href: string): string | null {
  const trimmed = href.trim();
  if (!/^https?:\/\//i.test(trimmed)) return null;
  try {
    const url = new URL(trimmed, "http://x.invalid");
    return url.protocol === "http:" || url.protocol === "https:" ? url.href : null;
  } catch {
    return null;
  }
}

function rawText(token: { readonly raw: string }): ReactNode {
  return t(token.raw);
}

function inline(tokens: readonly Token[] | undefined, depth: number): ReactNode[] {
  return (tokens ?? []).map((token, i) => <Inline key={i} token={token} depth={depth} />);
}

function Inline({ token, depth }: { readonly token: Token; readonly depth: number }): ReactNode {
  if (depth > MAX_DEPTH) return rawText(token);
  switch (token.type) {
    case "text":
    case "escape":
      return t((token as Tokens.Text).text);
    case "strong":
      return (
        <strong className="font-semibold">
          {inline((token as Tokens.Strong).tokens, depth + 1)}
        </strong>
      );
    case "em":
      return <em>{inline((token as Tokens.Em).tokens, depth + 1)}</em>;
    case "del":
      return <del>{inline((token as Tokens.Del).tokens, depth + 1)}</del>;
    case "codespan":
      return (
        <code className="rounded bg-surface-sunken px-1 font-mono text-xs">
          {t((token as Tokens.Codespan).text)}
        </code>
      );
    case "br":
      return <br />;
    case "link": {
      const link = token as Tokens.Link;
      const label = inline(link.tokens, depth + 1);
      const href = safeHref(link.href);
      const autolink = link.href === link.text;
      return (
        <>
          {href === null ? (
            label
          ) : (
            <a
              href={href}
              target="_blank"
              rel="noopener noreferrer nofollow"
              className="text-accent underline underline-offset-2"
            >
              {label}
            </a>
          )}
          {link.href === "" || (autolink && href !== null) ? null : (
            <span className="text-fg-muted"> ({t(link.href)})</span>
          )}
        </>
      );
    }
    default:
      // html, image and anything unknown: the source, as text.
      return rawText(token);
  }
}

function Cell({ cell, header }: { readonly cell: Tokens.TableCell; readonly header: boolean }) {
  const Tag = header ? "th" : "td";
  const align =
    cell.align === "center" ? "text-center" : cell.align === "right" ? "text-right" : "text-left";
  return (
    <Tag
      className={cn(
        "border border-border px-2 py-1",
        align,
        header && "bg-surface-sunken font-medium",
      )}
    >
      {inline(cell.tokens, 0)}
    </Tag>
  );
}

function blocks(tokens: readonly Token[], depth: number): ReactNode[] {
  return tokens.map((token, i) => <Block key={i} token={token} depth={depth} />);
}

function Block({ token, depth }: { readonly token: Token; readonly depth: number }): ReactNode {
  if (depth > MAX_DEPTH) return <p className="my-1 whitespace-pre-wrap">{rawText(token)}</p>;
  switch (token.type) {
    case "space":
    case "checkbox":
      return null;
    case "heading": {
      // Page title is the h1, so a document "# Title" is an h2.
      const Tag = `h${Math.min((token as Tokens.Heading).depth + 1, 6)}` as
        "h2" | "h3" | "h4" | "h5" | "h6";
      return (
        <Tag className="mt-3 mb-1 font-semibold">{inline((token as Tokens.Heading).tokens, 0)}</Tag>
      );
    }
    case "paragraph":
      return <p className="my-1">{inline((token as Tokens.Paragraph).tokens, 0)}</p>;
    case "text": {
      // A tight list item's text, or stray top-level text.
      const text = token as Tokens.Text;
      return <>{text.tokens ? inline(text.tokens, 0) : t(text.text)}</>;
    }
    case "list": {
      const list = token as Tokens.List;
      const Tag = list.ordered ? "ol" : "ul";
      const start = typeof list.start === "number" && list.start !== 1 ? list.start : undefined;
      return (
        <Tag
          className={cn("my-1 pl-6", list.ordered ? "list-decimal" : "list-disc")}
          {...(list.ordered && start !== undefined ? { start } : {})}
        >
          {list.items.map((item, i) => (
            <li key={i} className={cn(item.task && "list-none")}>
              {item.task ? (
                <input
                  type="checkbox"
                  checked={item.checked === true}
                  disabled
                  readOnly
                  aria-label={item.checked === true ? "done" : "not done"}
                  className="mr-2 align-middle"
                />
              ) : null}
              {blocks(item.tokens, depth + 1)}
            </li>
          ))}
        </Tag>
      );
    }
    case "blockquote":
      return (
        <blockquote className="my-1 border-l-[3px] border-border pl-3 text-fg-muted">
          {blocks((token as Tokens.Blockquote).tokens, depth + 1)}
        </blockquote>
      );
    case "code":
      return (
        <pre className="my-1 overflow-x-auto rounded-md bg-surface-sunken p-2 font-mono text-xs">
          <code>{t((token as Tokens.Code).text)}</code>
        </pre>
      );
    case "table": {
      const table = token as Tokens.Table;
      return (
        <div className="my-1 overflow-x-auto">
          <table className="border-collapse text-sm">
            <thead>
              <tr>
                {table.header.map((cell, i) => (
                  <Cell key={i} cell={cell} header />
                ))}
              </tr>
            </thead>
            <tbody>
              {table.rows.map((row, r) => (
                <tr key={r}>
                  {row.map((cell, i) => (
                    <Cell key={i} cell={cell} header={false} />
                  ))}
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      );
    }
    case "hr":
      return <hr className="my-2 border-border" />;
    default:
      // html, def and anything unknown: the source, as text.
      return token.raw.trim() === "" ? null : (
        <p className="my-1 whitespace-pre-wrap">{rawText(token)}</p>
      );
  }
}

/** Renders Markdown as React elements; never HTML. */
export function Markdown({ source, className }: MarkdownProps) {
  return <div className={cn("text-sm", className)}>{blocks(lexer(source), 0)}</div>;
}
