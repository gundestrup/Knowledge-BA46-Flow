#!/bin/sh
# docs/assets demo captures.
#
# review-flow is recorded from the REAL /knowledge UI via the e2e
# screencast stack (scripts/e2e/screencast-review.mjs — headless Chrome +
# CDP against a fixture corpus, no Ollama needed).
# cli-flow is a stylized stepped mock rendered by docs/assets/demo.html
# (a terminal transcript has no web UI to record), screenshotted per step
# with headless Chrome.
#
# All scenes are converted by the shared screencast-convert.mjs.
# Build-host tools: go, Chrome (CHROME_BIN), ffmpeg (libsvtav1), img2webp,
# webpinfo.
set -eu
page="docs/assets/demo.html"
outdir="docs/assets/demo"
demos="cli-flow"
steps=4
chrome="${CHROME_BIN:-/Applications/Google Chrome.app/Contents/MacOS/Google Chrome}"
[ -x "$chrome" ] || { echo "Chrome not found (set CHROME_BIN)" >&2; exit 1; }
command -v go >/dev/null 2>&1 || { echo "go not found" >&2; exit 1; }
command -v ffmpeg >/dev/null 2>&1 || { echo "ffmpeg not found" >&2; exit 1; }
command -v img2webp >/dev/null 2>&1 || { echo "img2webp not found" >&2; exit 1; }
command -v webpinfo >/dev/null 2>&1 || { echo "webpinfo not found" >&2; exit 1; }
mkdir -p "$outdir"

# 1. Real-UI loops: fixture corpus + real dashboard on camera.
# The raw takes keep every repaint (UI polling emits identical frames), so
# the animated formats run through mpdecimate first — the mp4 keeps the
# full take, the loop drops static stretches like the other screencasts.
for pair in "review:review-flow" "report-flow:report-flow"; do
  take=${pair%%:*}
  scene=${pair##*:}
  node "scripts/e2e/screencast-$take.mjs"
  rframes="data/screencast-frames/$take"
  dedup="data/demo-frames/$scene"
  mkdir -p "$dedup"
  rm -f "$dedup"/f*.png
  ffmpeg -hide_banner -loglevel error -y -framerate 10 \
    -i "$rframes/f%04d.png" -vf mpdecimate -fps_mode vfr "$dedup/f%04d.png"
  node scripts/e2e/screencast-convert.mjs "$dedup" \
    --name "$scene" --out "$outdir" --fps 1
  # Poster = the last captured frame (apply/results state).
  last=$(ls "$rframes"/f*.png | tail -1)
  cp "$last" "$outdir/$scene.png"
  echo "wrote $outdir/$scene.{png,mp4,apng,webp,avif,svg}"
done

# 2. Stepped mock scenes from docs/assets/demo.html.
for demo in $demos; do
  frames="data/demo-frames/$demo"
  mkdir -p "$frames"
  rm -f "$frames"/f*.png
  i=0
  while [ "$i" -lt "$steps" ]; do
    n=$(printf %04d "$i")
    if ! "$chrome" --headless=new --disable-gpu --hide-scrollbars \
      --force-device-scale-factor=1 \
      --screenshot="$frames/f$n.png" --window-size=880,560 \
      "file://$PWD/$page?demo=$demo&step=$i" >/dev/null 2>&1; then
      echo "Chrome screenshot failed ($demo step $i)" >&2
      exit 1
    fi
    i=$((i + 1))
  done
  cp "$frames/f0000.png" "$outdir/$demo.png"
  node scripts/e2e/screencast-convert.mjs "$frames" \
    --name "$demo" --out "$outdir" --fps 0.5
  echo "wrote $outdir/$demo.{png,apng,webp,avif,svg}"
done
