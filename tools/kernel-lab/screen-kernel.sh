#!/usr/bin/env sh
# Check a kernel file for correctness on a laptop without a GPU:
#   tools/kernel-lab/screen-kernel.sh residual-rmsnorm path/to/kernel.py
# Add --verification to use the separate verification shapes. Exits 1 when the
# kernel fails. Needs Docker; the first run builds the image.
set -eu

if [ $# -lt 2 ]; then
  echo "usage: $0 FAMILY KERNEL.py [--verification]" >&2
  exit 2
fi
here=$(CDPATH= cd -- "$(dirname "$0")" && pwd)
verification=false
[ "${3:-}" = "--verification" ] && verification=true
result=$(mktemp)
trap 'rm -f "$result"' EXIT

python3 -c '
import json, sys
family, path, verification = sys.argv[1], sys.argv[2], sys.argv[3] == "true"
print(json.dumps({"family": family, "source": open(path, encoding="utf-8").read(), "verification": verification}))
' "$1" "$2" "$verification" | "$here/run-cpu-screen.sh" >"$result"
python3 "$here/screen_report.py" <"$result"
