#!/bin/sh
set -eu

root=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)
static="$root/internal/dashboard/static"

go test -count=1 ./internal/dashboard ./internal/gateway
node --test "$root/internal/dashboard/model_test.mjs"
node --check "$static/app.mjs"
node --check "$static/model.mjs"

# Keep security, accessibility and responsive constraints executable without
# introducing a browser framework into the production asset pipeline.
grep -q 'href="#connection-panel"' "$static/index.html"
grep -q 'aria-live="polite"' "$static/index.html"
grep -q 'prefers-reduced-motion' "$static/style.css"
grep -q '@media (max-width: 700px)' "$static/style.css"
grep -q '@media (max-width: 420px)' "$static/style.css"
if grep -Eq 'innerHTML|outerHTML|insertAdjacentHTML|localStorage|document\.cookie|eval\(' "$static/app.mjs"; then
  echo "Dashboard uses a forbidden DOM or credential persistence primitive." >&2
  exit 1
fi
