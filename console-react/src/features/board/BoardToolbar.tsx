import { Search } from "lucide-react";

import {
  type RequestBoardFilters,
  type RequestBoardSection,
  sectionLabels,
} from "@/domain/boardFilters";
import { Input } from "@/ui/Input";

import { FilterChip } from "./FilterChip";
import { FreshnessIndicator, type FreshnessIndicatorProps } from "./FreshnessIndicator";

const SECTIONS: readonly RequestBoardSection[] = ["needsYou", "working", "finished"];

export interface BoardToolbarProps {
  readonly filters: RequestBoardFilters;
  readonly searchText: string;
  readonly allProjects: readonly string[];
  readonly freshness: FreshnessIndicatorProps["freshness"];
  readonly lastUpdateAt: number;
  readonly onSearch: (value: string) => void;
  readonly onToggleSection: (section: RequestBoardSection) => void;
  readonly onToggleProject: (project: string) => void;
}

/** Search, the section filter, the project filter and the live indicator. */
export function BoardToolbar({
  filters,
  searchText,
  allProjects,
  freshness,
  lastUpdateAt,
  onSearch,
  onToggleSection,
  onToggleProject,
}: BoardToolbarProps) {
  return (
    <div className="flex flex-col gap-3">
      <div className="flex flex-wrap items-center gap-3">
        <div className="relative w-full max-w-sm">
          <Search
            aria-hidden
            className="text-fg-subtle pointer-events-none absolute top-1/2 left-2.5 size-4 -translate-y-1/2"
          />
          <Input
            type="search"
            aria-label="Search id, title, workspace"
            placeholder="Search id, title, workspace"
            value={searchText}
            onChange={(event) => {
              onSearch(event.target.value);
            }}
            className="pl-8"
          />
        </div>
        <div role="group" aria-label="Section" className="flex flex-wrap items-center gap-1.5">
          {SECTIONS.map((section) => (
            <FilterChip
              key={section}
              pressed={filters.section === section}
              onPressedChange={() => {
                onToggleSection(section);
              }}
            >
              {sectionLabels[section]}
            </FilterChip>
          ))}
        </div>
        <div className="ml-auto">
          <FreshnessIndicator freshness={freshness} lastUpdateAt={lastUpdateAt} />
        </div>
      </div>
      {allProjects.length > 0 ? (
        <div role="group" aria-label="Project" className="flex flex-wrap items-center gap-1.5">
          {allProjects.map((project) => (
            <FilterChip
              key={project}
              pressed={filters.projects.has(project)}
              onPressedChange={() => {
                onToggleProject(project);
              }}
            >
              {project}
            </FilterChip>
          ))}
        </div>
      ) : null}
    </div>
  );
}
