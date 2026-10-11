import { Bell, BellOff } from "lucide-react";

import type { NotificationSupport } from "@/platform/browserNotifications";
import { Button } from "@/ui/Button";

export interface NotificationToggleProps {
  readonly support: NotificationSupport;
  readonly on: boolean;
  readonly onTurnOn: () => void;
  readonly onTurnOff: () => void;
}

/**
 * The sidebar footer's switch for browser notifications. Absent where the
 * browser has none; when the browser blocks them the operator can only change
 * that in the browser's site settings, so it says so instead of offering a
 * button that does nothing.
 */
export function NotificationToggle({ support, on, onTurnOn, onTurnOff }: NotificationToggleProps) {
  if (support === "unsupported") return null;
  if (support === "denied") {
    return (
      <span
        className="flex min-w-0 items-center gap-1.5 px-1.5 text-xs text-fg-subtle max-md:block max-md:truncate"
        title="Allow notifications for this site in the browser's site settings, then reload."
      >
        <BellOff aria-hidden className="size-4 shrink-0 max-md:mr-1.5 max-md:inline" />
        Notifications blocked in this browser
      </span>
    );
  }
  const enabled = support === "granted" && on;
  return (
    <Button
      variant="ghost"
      size="sm"
      onClick={enabled ? onTurnOff : onTurnOn}
      title={
        enabled
          ? "This tab tells you when a request needs you. Click to turn off."
          : "Get a browser notification when a request needs you."
      }
    >
      <Bell aria-hidden />
      {enabled ? "Notifications on" : "Turn on notifications"}
    </Button>
  );
}
