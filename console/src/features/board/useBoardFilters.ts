import { useMemo, useState } from "react";
import { useSearchParams } from "react-router";

import {
  type BoardWindowDays,
  type RequestBoardFilters,
  type RequestBoardSection,
  copyRequestBoardFilters,
  filtersFromSearchParams,
  toQueryParameters,
} from "@/domain/boardFilters";

export interface BoardFilterControls {
  readonly filters: RequestBoardFilters;
  /** The search box's text: local so typing never fights the URL round trip. */
  readonly searchText: string;
  readonly setSearch: (value: string) => void;
  /** Selecting the section already selected clears it. */
  readonly toggleSection: (section: RequestBoardSection) => void;
  readonly selectSection: (section: RequestBoardSection | null) => void;
  readonly toggleProject: (project: string) => void;
  /** The window on finished work: 7 days, 30 days or all. */
  readonly selectDays: (days: BoardWindowDays) => void;
}

function toSearchParams(filters: RequestBoardFilters): URLSearchParams {
  const params = new URLSearchParams();
  for (const [key, value] of Object.entries(toQueryParameters(filters))) {
    if (typeof value === "string") params.set(key, value);
    else for (const item of value) params.append(key, item);
  }
  return params;
}

/**
 * The board's filters, kept in the URL (?project=&group=&q=&days=) so a filtered
 * board is a shareable link. Changes replace the history entry: Back leaves
 * the board instead of walking through every keystroke.
 */
export function useBoardFilters(): BoardFilterControls {
  const [params, setParams] = useSearchParams();
  const filters = useMemo(() => filtersFromSearchParams(params), [params]);
  const [searchText, setSearchText] = useState(filters.search);
  const [seenSearch, setSeenSearch] = useState(filters.search);
  // Adopt a search changed from outside (Back/Forward, a pasted link) without
  // fighting the operator's own typing, which sets both values together.
  if (filters.search !== seenSearch) {
    setSeenSearch(filters.search);
    setSearchText(filters.search);
  }

  const apply = (next: RequestBoardFilters): void => {
    setParams(toSearchParams(next), { replace: true });
  };
  const selectSection = (section: RequestBoardSection | null): void => {
    apply(copyRequestBoardFilters(filters, { section }));
  };
  return {
    filters,
    searchText,
    setSearch: (value) => {
      setSearchText(value);
      setSeenSearch(value);
      apply(copyRequestBoardFilters(filters, { search: value }));
    },
    selectSection,
    toggleSection: (section) => {
      selectSection(filters.section === section ? null : section);
    },
    toggleProject: (project) => {
      const projects = new Set(filters.projects);
      if (!projects.delete(project)) projects.add(project);
      apply(copyRequestBoardFilters(filters, { projects }));
    },
    selectDays: (days) => {
      apply(copyRequestBoardFilters(filters, { days }));
    },
  };
}
