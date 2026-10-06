// The tab icon's "needs you" badge: a small red count drawn onto the 32px
// icon, so a board left in a background tab shows the count without its
// title being readable. Drawing is plain browser canvas work.

const baseHref = "/favicon.png";
const size = 32;

function iconLink(): HTMLLinkElement | null {
  // index.html marks the PNG link: its rel changes while a badge is shown.
  return document.querySelector<HTMLLinkElement>("link[data-tab-icon]");
}

/** "7", or "99+" above 99. */
export function badgeLabel(count: number): string {
  return count > 99 ? "99+" : String(count);
}

/**
 * Shows `count` on the tab icon, or restores the plain icon for zero. The
 * PNG link becomes the page's icon while a badge is shown, since a browser
 * prefers the SVG link otherwise.
 */
export function setFaviconBadge(count: number): void {
  const link = iconLink();
  if (!link) return;
  if (count <= 0) {
    link.rel = "alternate icon";
    link.href = baseHref;
    return;
  }
  const image = new Image();
  image.onload = () => {
    const canvas = document.createElement("canvas");
    canvas.width = size;
    canvas.height = size;
    const context = canvas.getContext("2d");
    if (!context) return;
    context.drawImage(image, 0, 0, size, size);
    const radius = 10;
    const x = size - radius - 1;
    const y = radius + 1;
    context.beginPath();
    context.arc(x, y, radius, 0, 2 * Math.PI);
    context.fillStyle = "#c62828";
    context.fill();
    context.fillStyle = "#ffffff";
    context.font = "bold 12px sans-serif";
    context.textAlign = "center";
    context.textBaseline = "middle";
    context.fillText(badgeLabel(count), x, y + 1);
    link.rel = "icon";
    link.href = canvas.toDataURL("image/png");
  };
  image.src = baseHref;
}
