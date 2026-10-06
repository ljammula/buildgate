// What an oracle review has displayed. Approval pins every file by hash and
// the server refuses one that does not name each file the operator was shown,
// so a file counts as "shown" only while its tile is open, its content is
// loaded, and that content's hash (computed over the bytes as received)
// equals the listing's. Content is fetched per (file, listed hash): a changed
// listing is a different query, so stale bytes are never shown for a new
// hash, and a collapsed file is never refetched.
import { useQueries } from "@tanstack/react-query";
import { useState } from "react";

import type { FetchedOracleFile } from "@/api/oracle";
import { shownOracleFiles } from "@/domain/oracle";

/** One listed file, as the store needs it. */
export interface OracleFileSource {
  /** Opaque, unique across the store; the approval key for a ticket's files. */
  readonly key: string;
  /** The listing's hash for it. */
  readonly sha256: string;
  /** TanStack Query key of its content (the store appends the listed hash). */
  readonly queryKey: readonly unknown[];
  readonly fetch: (signal: AbortSignal) => Promise<FetchedOracleFile>;
}

/** What a file tile reads and does. */
export interface OracleFileStore {
  isOpen(key: string): boolean;
  setOpen(key: string, open: boolean): void;
  /** Marks `key` open (after an edit); its new content is fetched once the listing's hash moves. */
  reopen(key: string): void;
  content(key: string): FetchedOracleFile | null;
  error(key: string): unknown;
  /** key -> listed hash for every listed file that is currently shown. */
  readonly shown: ReadonlyMap<string, string>;
}

/**
 * @param sources every listed file (empty while no listing has loaded)
 * @param alwaysLoad keys fetched even while collapsed (MANIFEST.json, whose
 * content feeds the coverage list); fetching never counts as showing.
 */
export function useOracleFileStore(
  sources: readonly OracleFileSource[],
  alwaysLoad: readonly string[] = [],
): OracleFileStore {
  const [opened, setOpened] = useState<ReadonlySet<string>>(new Set());
  const listed = new Map(sources.map((s) => [s.key, s.sha256]));
  // A tile of a file the listing no longer has is closed.
  const open = new Set([...opened].filter((key) => listed.has(key)));
  const wanted = sources.filter((s) => open.has(s.key) || alwaysLoad.includes(s.key));

  const results = useQueries({
    queries: wanted.map((s) => ({
      queryKey: [...s.queryKey, s.sha256],
      queryFn: ({ signal }: { signal: AbortSignal }) => s.fetch(signal),
      retry: false,
      staleTime: Infinity,
      refetchOnWindowFocus: false,
    })),
  });

  const contents = new Map<string, FetchedOracleFile>();
  const errors = new Map<string, unknown>();
  wanted.forEach((s, i) => {
    const result = results[i];
    if (result?.data !== undefined) contents.set(s.key, result.data);
    else if (result?.isError === true) errors.set(s.key, result.error);
  });
  const hashes = new Map([...contents].map(([key, file]) => [key, file.sha256]));
  const shown = shownOracleFiles(listed, open, hashes);

  return {
    isOpen: (key) => open.has(key),
    setOpen: (key, isOpen) => {
      setOpened((previous) => {
        const next = new Set(previous);
        if (isOpen) next.add(key);
        else next.delete(key);
        return next;
      });
    },
    reopen: (key) => {
      setOpened((previous) => new Set(previous).add(key));
    },
    content: (key) => contents.get(key) ?? null,
    error: (key) => errors.get(key),
    shown,
  };
}
