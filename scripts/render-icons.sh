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
#   console/web/favicon.png                 32px tab icon (the "needs you"
#                                           badge in index.html draws at 32)
#   console/web/icons/Icon-{192,512}.png    rounded tile, transparent corners
#   console/web/icons/Icon-maskable-*.png   full-bleed tile, mark scaled to
#                                           80% so it sits inside the
#                                           maskable safe zone
#   internal/notify/buildgate-icon.png      256px, embedded into factoryd for
#                                           desktop notifications' -contentImage
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

# The maskable variant: square the tile's corners and shrink the mark about
# the centre, so a launcher's circle/squircle mask never clips it.
sed -e 's/rx="112"/rx="0"/' \
	-e 's|<g id="mark">|<g id="mark" transform="translate(51.2 51.2) scale(0.8)">|' \
	"$src" >"$work/maskable.svg"

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

render "$src" 32 "$root/console/web/favicon.png"
render "$src" 192 "$root/console/web/icons/Icon-192.png"
render "$src" 512 "$root/console/web/icons/Icon-512.png"
render "$work/maskable.svg" 192 "$root/console/web/icons/Icon-maskable-192.png"
render "$work/maskable.svg" 512 "$root/console/web/icons/Icon-maskable-512.png"
render "$src" 256 "$root/internal/notify/buildgate-icon.png"
