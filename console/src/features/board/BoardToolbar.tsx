import { Search } from "lucide-react";

import {
  type BoardWindowDays,
  type RequestBoardFilters,
  type RequestBoardSection,
  boardWindowChoices,
  sectionLabels,
} from "@/domain/boardFilters";
import { boardWindowChoiceLabel } from "@/domain/boardWindow";
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
  readonly onSelectDays: (days: BoardWindowDays) => void;
}

/** Search, the section filter, the window on finished work, the project filter (only with more than one project to choose) and the live indicator. */
export function BoardToolbar({
  filters,
  searchText,
  allProjects,
  freshness,
  lastUpdateAt,
  onSearch,
  onToggleSection,
  onToggleProject,
  onSelectDays,
}: BoardToolbarProps) {
  return (
    <div className="flex flex-col gap-2">
      <div className="flex flex-wrap items-center gap-x-3 gap-y-2">
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
        <div className="flex flex-wrap items-center gap-1.5">
          <span aria-hidden className="text-fg-muted text-xs">
            Show:
          </span>
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
        </div>
        {/* One of three, always: pressing the one in use changes nothing. */}
        <div className="flex flex-wrap items-center gap-1.5">
          <span aria-hidden className="text-fg-muted text-xs">
            Finished:
          </span>
          <div
            role="group"
            aria-label="Finished work from the last"
            title="How far back finished work, activity and the numbers go. Work in flight is always shown."
            className="flex items-center [&>button]:relative [&>button]:rounded-none [&>button:first-child]:rounded-l-md [&>button:last-child]:rounded-r-md [&>button+button]:-ml-px [&>button[aria-pressed=true]]:z-10"
          >
            {boardWindowChoices.map((days) => (
              <FilterChip
                key={days}
                pressed={filters.days === days}
                onPressedChange={() => {
                  onSelectDays(days);
                }}
              >
                {boardWindowChoiceLabel(days)}
              </FilterChip>
            ))}
          </div>
        </div>
        {allProjects.length > 1 ? (
          <div className="flex flex-wrap items-center gap-1.5">
            <span aria-hidden className="text-fg-muted text-xs">
              Project:
            </span>
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
          </div>
        ) : null}
        <div className="ml-auto">
          <FreshnessIndicator freshness={freshness} lastUpdateAt={lastUpdateAt} />
        </div>
      </div>
    </div>
  );
}
