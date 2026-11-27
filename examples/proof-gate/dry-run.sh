#!/bin/sh
# Dry run of the proof gate: the real CLI and the real GitHub Action against a
# fake control plane. No GPU, no spend, no GitHub. It reads one finished run
# per verdict, then proves current.yaml -> proposed.yaml the way a pull
# request does, and verifies the signed report offline.
set -eu

root=$(CDPATH= cd -- "$(dirname "$0")/../.." && pwd)
here="$root/examples/proof-gate"
work=$(mktemp -d)
fake_pid=
cleanup() {
  [ -n "$fake_pid" ] && kill "$fake_pid" 2>/dev/null || true
  rm -rf "$work"
}
trap cleanup EXIT HUP INT TERM

(cd "$root" && go build -o "$work/infercrane" ./cmd/infercrane && go build -o "$work/fakeproofcontrol" ./internal/testtools/fakeproofcontrol)
"$work/fakeproofcontrol" -api-key dry-run-key -on-create pass -running-reads 0 -ready-file "$work/ready.json" >/dev/null 2>"$work/fake.log" &
fake_pid=$!
tries=0
until [ -s "$work/ready.json" ]; do
  tries=$((tries + 1))
  if [ "$tries" -gt 100 ]; then
    echo "fake control plane did not start" >&2
    cat "$work/fake.log" >&2
    exit 1
  fi
  sleep 0.1
done
field() { node -e 'process.stdout.write(JSON.parse(require("fs").readFileSync(process.argv[1], "utf8"))[process.argv[2]])' "$work/ready.json" "$1"; }
INFERCRANE_URL=$(field url)
INFERCRANE_API_KEY=dry-run-key
INFERCRANE_CONFIG="$work/no-config.json"
export INFERCRANE_URL INFERCRANE_API_KEY INFERCRANE_CONFIG
public_key=$(field public_key)

# run_action NAME [VAR=VALUE ...] runs the action as GitHub would and prints
# the verdict line of its job summary.
run_action() {
  name=$1
  shift
  status=0
  env INPUT_MODE=proof-gate INPUT_CLI_PATH="$work/infercrane" INPUT_COMMENT=false \
    INPUT_VERSION=v0.0.0 INPUT_ON_WATCH=neutral INPUT_PROOF_TIMEOUT=1m \
    INPUT_OUTPUT_PATH="$work/$name.json" INPUT_REPORT_PATH="$work/$name.report.json" \
    GITHUB_OUTPUT="$work/$name.out" GITHUB_STEP_SUMMARY="$work/$name.md" RUNNER_TEMP="$work" \
    "$@" node "$root/actions/infercrane/index.js" >"$work/$name.log" 2>&1 || status=$?
  verdict=$(grep -m1 '^### ' "$work/$name.md" 2>/dev/null | sed 's/^### //' || true)
  printf '%-16s exit %s  %s\n' "$name" "$status" "${verdict:-$(tail -n 1 "$work/$name.log")}"
  echo "$status" >"$work/$name.status"
}

expect() {
  actual=$(cat "$work/$1.status")
  if [ "$actual" != "$2" ]; then
    echo "$1: exit $actual, want $2" >&2
    cat "$work/$1.log" >&2
    exit 1
  fi
}

echo "== Read a finished run's verdict by ID"
for scenario in pass gain watch block answers-blocked unknown; do
  run_action "$scenario" INPUT_CAMPAIGN="run-$scenario" INPUT_PUBLIC_KEY="$public_key"
done
expect pass 0
expect gain 0
expect watch 0
expect block 1
expect answers-blocked 1
expect unknown 1

echo
echo "== Prove a pull request's change: current.yaml on the base commit, proposed.yaml at the head"
repo="$work/repo"
mkdir -p "$repo/deploy"
cp "$here/current.yaml" "$repo/deploy/qwen.yaml"
git -C "$repo" init -q
git -C "$repo" -c user.email=dry-run@example.invalid -c user.name=dry-run add deploy/qwen.yaml
git -C "$repo" -c user.email=dry-run@example.invalid -c user.name=dry-run commit -q -m "current setup"
base=$(git -C "$repo" rev-parse HEAD)
cp "$here/proposed.yaml" "$repo/deploy/qwen.yaml"
(cd "$repo" && run_action pull-request INPUT_SPEC=deploy/qwen.yaml INPUT_BASE_REF="$base" INPUT_MAX_COST_USD=20 INPUT_APPROVAL_WINDOW=3h INPUT_OBJECTIVE=latency INPUT_PUBLIC_KEY="$public_key")
expect pull-request 0
grep -q '^run-pass$' "$work/pull-request.out"

echo
echo "== Check the saved report offline, without the control plane"
"$work/infercrane" optimize report verify "$work/pull-request.report.json" --public-key "$public_key"
"$work/infercrane" optimize gate --file "$work/pull-request.report.json" --public-key "$public_key" | head -n 2

echo
echo "== The pull-request comment"
cat "$work/pull-request.md"
