#!/bin/sh
# render-icons.sh: regenerate every Buildgate icon PNG from the one source,
# assets/brand/buildgate.svg. Run it after editing the SVG and commit the
# PNGs it writes -- neither `make verify` nor `make console-build` renders
# them, so a build machine needs no browser.
#
# Rasterizes with headless Chrome (the one SVG renderer every dev Mac here
# already has; no rsvg-convert/ImageMagick/cairosvg dependency). Override
# its location with CHROME=/path/to/chrome.
#
# Outputs:
#   internal/notify/buildgate-icon.png      256px, embedded into factoryd for
#                                           desktop notifications' -contentImage
#   console/public/buildgate.svg            the mark, copied: the console's
#                                           sidebar mark and tab icon
#   console/public/favicon.png              32px, the tab icon the "needs you"
#                                           badge is drawn onto
set -eu

root=$(cd "$(dirname "$0")/.." && pwd)
src="$root/assets/brand/buildgate.svg"
chrome=${CHROME:-"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"}

if [ ! -x "$chrome" ]; then
	echo "render-icons: Chrome not found at $chrome (set CHROME=...)" >&2
	exit 1
fi

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# render SVG SIZE OUT -- the SVG is inlined into the page rather than
# referenced by <img src>, which headless Chrome can screenshot before the
# image has loaded (observed: blank PNGs).
render() {
	{
		printf '<html><head><style>body{margin:0}svg{display:block;width:%spx;height:%spx}</style></head><body>' "$2" "$2"
		cat "$1"
		printf '</body></html>'
	} >"$work/page.html"
	"$chrome" --headless=new --disable-gpu --hide-scrollbars \
		--default-background-color=00000000 \
		--window-size="$2,$2" --screenshot="$3" "file://$work/page.html" >/dev/null 2>&1
	echo "wrote $3"
}

render "$src" 256 "$root/internal/notify/buildgate-icon.png"

# The console (console/public is served at the bundle's root):
# the mark itself for the sidebar and the tab icon, and a 32px PNG the
# "needs you" tab badge is drawn onto.
mkdir -p "$root/console/public"
cp "$src" "$root/console/public/buildgate.svg"
echo "wrote $root/console/public/buildgate.svg"
render "$src" 32 "$root/console/public/favicon.png"
