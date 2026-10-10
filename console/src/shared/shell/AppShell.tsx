import {
  Activity,
  FolderGit2,
  LayoutDashboard,
  ListChecks,
  Monitor,
  Moon,
  Plus,
  Sun,
} from "lucide-react";
import type { ComponentType, ReactNode } from "react";
import { Link, matchPath, useLocation } from "react-router";

import { useApi } from "@/api/ApiProvider";
import { nextThemeMode, useThemeMode } from "@/platform/theme";
import type { ThemeMode } from "@/platform/themeStore";
import { boardPath, newRequestPath, projectsPath, runsPath, triagePath } from "@/routes/paths";
import { BrandMark } from "@/ui/BrandMark";
import { Button } from "@/ui/Button";
import { IconButton } from "@/ui/IconButton";
import { NavCount } from "@/shared/shell/NavCount";
import { NotificationToggle } from "@/shared/shell/NotificationToggle";
import { useBrowserNotifier } from "@/shared/shell/useBrowserNotifier";
import { useNeedsYouCount } from "@/shared/shell/useNeedsYouCount";
import { cn } from "@/ui/cn";

interface NavItem {
  readonly to: string;
  readonly label: string;
  readonly icon: ComponentType<{ className?: string }>;
  /** Match the path exactly: "/" would otherwise match every page. */
  readonly end?: boolean;
}

const navItems: readonly NavItem[] = [
  { to: boardPath(), label: "Mission Control", icon: LayoutDashboard, end: true },
  { to: triagePath(), label: "Triage", icon: ListChecks },
  { to: runsPath(), label: "Runs", icon: Activity },
  { to: projectsPath(), label: "Projects", icon: FolderGit2 },
];

// The toggle's names are fixed so operators and the browser walk find the same control.
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
    <IconButton
      label={themeLabels[mode]}
      onClick={() => {
        setMode(nextThemeMode(mode));
      }}
    >
      <Icon aria-hidden />
    </IconButton>
  );
}

export interface AppShellProps {
  readonly children: ReactNode;
}

/**
 * The frame around every screen: one sidebar holding the primary action, the
 * navigation, and what is true on every page (whether this console may
 * write, the theme).
 */
export function AppShell({ children }: AppShellProps) {
  const { canWrite } = useApi();
  const needsYou = useNeedsYouCount();
  const notifier = useBrowserNotifier();
  const { pathname } = useLocation();
  return (
    <div className="grid min-h-screen grid-cols-[14rem_minmax(0,1fr)] max-md:grid-cols-1">
      <aside className="sticky top-0 flex h-screen flex-col gap-3 border-r border-border bg-surface p-3 max-md:static max-md:h-auto">
        <Link
          to={boardPath()}
          className="flex h-8 items-center gap-2.5 rounded-md px-1.5 font-semibold tracking-tight text-fg"
        >
          <BrandMark className="size-6" />
          Buildgate
        </Link>
        <Button asChild variant="primary" className="w-full">
          <Link to={newRequestPath()}>
            <Plus aria-hidden />
            New request
          </Link>
        </Button>
        {/* The count pills are decoration for the eye; this is the same fact for assistive technology. */}
        <p role="status" className="sr-only">
          {needsYou === null
            ? ""
            : needsYou === 0
              ? "Nothing is waiting on you."
              : `${needsYou} ${needsYou === 1 ? "request needs" : "requests need"} you.`}
        </p>
        <nav aria-label="Main" className="flex flex-1 flex-col gap-0.5 max-md:flex-row">
          {navItems.map(({ to, label, icon: Icon, end }) => {
            const isActive = matchPath({ path: to, end: end ?? false }, pathname) !== null;
            return (
              <Link
                key={to}
                to={to}
                aria-current={isActive ? "page" : undefined}
                className={cn(
                  "flex h-8 items-center gap-2.5 rounded-md px-2.5 text-fg-muted transition-colors hover:bg-surface-hover hover:text-fg",
                  isActive && "bg-surface-hover font-medium text-fg",
                )}
              >
                <Icon
                  aria-hidden
                  className={cn("size-4", isActive ? "text-accent" : "text-fg-subtle")}
                />
                {label}
                {to === triagePath() ? <NavCount count={needsYou} /> : null}
              </Link>
            );
          })}
        </nav>
        <div className="flex flex-col gap-2 border-t border-border pt-3">
          <NotificationToggle
            support={notifier.support}
            on={notifier.on}
            onTurnOn={notifier.turnOn}
            onTurnOff={notifier.turnOff}
          />
          <div className="flex items-center justify-between gap-2">
            <span
              className={cn(
                "flex items-center gap-1.5 px-1.5 text-xs",
                canWrite ? "text-fg-subtle" : "text-tone-warning",
              )}
              title={
                canWrite
                  ? "This console can approve, reject and start work."
                  : "This server accepts no writes from this console. Open the link printed by factoryd serve, or use the CLI."
              }
            >
              <span
                aria-hidden
                className={cn(
                  "size-1.5 rounded-full",
                  canWrite ? "bg-tone-success" : "bg-tone-warning",
                )}
              />
              {canWrite ? "Read and write" : "Read-only"}
            </span>
            <ThemeToggle />
          </div>
        </div>
      </aside>
      <main className="min-w-0">{children}</main>
    </div>
  );
}
