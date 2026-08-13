#!/bin/sh
set -eu

root=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)
cast=$root/docs/images/showcase/connect-existing.cast
gif=$root/docs/images/showcase/connect-existing.gif

command -v asciinema >/dev/null 2>&1 || {
  echo "asciinema is required to record the terminal demo" >&2
  exit 1
}
command -v agg >/dev/null 2>&1 || {
  echo "agg is required to render the terminal demo GIF" >&2
  exit 1
}

mkdir -p "$(dirname "$cast")"
cd "$root"
asciinema record --quiet --headless --overwrite --return \
  --window-size 112x34 --idle-time-limit 1.5 \
  --title 'Connect existing inference and inspect evidence' \
  --command './scripts/demo-connect.sh' "$cast"
agg --theme github-dark --font-size 15 --speed 1.35 "$cast" "$gif"

echo "Terminal demo recorded"
echo "Cast  $cast"
echo "GIF   $gif"
