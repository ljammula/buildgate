// Opens links such as log paths and the Temporal UI in a separate tab.

/** Opens a URL in a new browser tab. */
export function openInNewTab(url: string): void {
  try {
    window.open(url, "_blank");
  } catch {
    // A blocked or unavailable browser window has no useful fallback here.
  }
}
