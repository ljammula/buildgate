import {
  Activity,
  FolderGit2,
  Inbox,
  ListChecks,
  Monitor,
  Moon,
  Plus,
  ServerCog,
  Sun,
} from "lucide-react";
import type { ComponentType, ReactNode } from "react";
import { Link, NavLink } from "react-router";

import { useApi } from "@/api/ApiProvider";
import { nextThemeMode, useThemeMode } from "@/platform/theme";
import type { ThemeMode } from "@/platform/themeStore";
import {
  boardPath,
  newRequestPath,
  opsPath,
  projectsPath,
  runsPath,
  triagePath,
} from "@/routes/paths";
import { Button } from "@/ui/Button";
import { cn } from "@/ui/cn";

interface NavItem {
  readonly to: string;
  readonly label: string;
  readonly icon: ComponentType<{ className?: string }>;
  /** Match the path exactly: "/" would otherwise match every page. */
  readonly end?: boolean;
}

const navItems: readonly NavItem[] = [
  { to: boardPath(), label: "Requests", icon: Inbox, end: true },
  { to: triagePath(), label: "Triage", icon: ListChecks },
  { to: runsPath(), label: "Runs", icon: Activity },
  { to: projectsPath(), label: "Projects", icon: FolderGit2 },
  { to: opsPath(), label: "Ops", icon: ServerCog },
];

// The toggle's names are the Flutter console's, so operators and the shared
// browser scenarios find the same control.
const themeLabels: Readonly<Record<ThemeMode, string>> = {
  system: "Theme: system (tap for light)",
  light: "Theme: light (tap for dark)",
  dark: "Theme: dark (tap for system)",
};

const themeIcons: Readonly<Record<ThemeMode, ComponentType<{ className?: string }>>> = {
  system: Monitor,
  light: Sun,
  dark: Moon,
};

function ThemeToggle() {
  const [mode, setMode] = useThemeMode();
  const Icon = themeIcons[mode];
  return (
    <Button
      variant="ghost"
      size="icon"
      aria-label={themeLabels[mode]}
      title={themeLabels[mode]}
      onClick={() => {
        setMode(nextThemeMode(mode));
      }}
    >
      <Icon aria-hidden />
    </Button>
  );
}

export interface AppShellProps {
  readonly children: ReactNode;
}

/**
 * The frame around every screen: the navigation rail, and the bar that holds
 * what is true on every page (whether this console may write, the theme).
 */
export function AppShell({ children }: AppShellProps) {
  const { canWrite } = useApi();
  return (
    <div className="grid min-h-screen grid-cols-[13rem_1fr] max-md:grid-cols-1">
      <aside className="sticky top-0 flex h-screen flex-col border-r border-border bg-surface max-md:static max-md:h-auto">
        <Link
          to={boardPath()}
          className="flex h-12 items-center gap-2 border-b border-border px-4 font-semibold tracking-tight"
        >
          <span
            aria-hidden
            className="grid size-5 place-items-center rounded-sm bg-accent text-[11px] font-bold text-accent-fg"
          >
            B
          </span>
          Buildgate
        </Link>
        <nav aria-label="Main" className="flex flex-1 flex-col gap-0.5 p-2 max-md:flex-row">
          {navItems.map(({ to, label, icon: Icon, end }) => (
            <NavLink
              key={to}
              to={to}
              end={end ?? false}
              className={({ isActive }) =>
                cn(
                  "flex h-8 items-center gap-2 rounded-md px-2.5 text-fg-muted transition-colors hover:bg-surface-hover hover:text-fg",
                  isActive && "bg-accent-soft font-medium text-fg",
                )
              }
            >
              <Icon aria-hidden className="size-4" />
              {label}
            </NavLink>
          ))}
        </nav>
        <div className="flex items-center justify-between gap-2 border-t border-border p-2">
          <span
            className={cn("px-1.5 text-xs", canWrite ? "text-fg-subtle" : "text-tone-warning")}
            title={
              canWrite
                ? "This console can approve, reject and start work."
                : "This server accepts no writes from this console. Open the link printed by `factoryd serve`, or use the CLI."
            }
          >
            {canWrite ? "Read and write" : "Read-only"}
          </span>
          <ThemeToggle />
        </div>
      </aside>
      <div className="flex min-w-0 flex-col">
        <div className="flex h-12 items-center justify-end gap-2 border-b border-border bg-surface px-6">
          <Button asChild variant="primary" size="sm">
            <Link to={newRequestPath()}>
              <Plus aria-hidden />
              New request
            </Link>
          </Button>
        </div>
        <main className="min-w-0 flex-1">{children}</main>
      </div>
    </div>
  );
}
