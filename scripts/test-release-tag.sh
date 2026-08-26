#!/bin/sh
set -eu

root=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)
temporary=$(mktemp -d)
trap 'rm -rf "$temporary"' EXIT HUP INT TERM

mkdir -p "$temporary/repository/scripts" "$temporary/repository/.release"
cp "$root/scripts/release-tag.sh" "$temporary/repository/scripts/release-tag.sh"
cat >"$temporary/repository/.release/version.json" <<'EOF'
{
  "schema_version": 1,
  "version": "1.0.0",
  "candidate_tag": "v1.0.0-rc.1",
  "stable_tag": "v1.0.0"
}
EOF

git -C "$temporary/repository" init -q -b main
git -C "$temporary/repository" config user.name "InferCrane release test"
git -C "$temporary/repository" config user.email "release-test@infercrane.invalid"
printf 'release candidate\n' >"$temporary/repository/product"
git -C "$temporary/repository" add .
git -C "$temporary/repository" commit -q -m "prepare release"
git -C "$temporary/repository" update-ref refs/remotes/origin/main HEAD

"$temporary/repository/scripts/release-tag.sh" check >/dev/null

git -C "$temporary/repository" switch -q -c unsafe-branch
if "$temporary/repository/scripts/release-tag.sh" check >/dev/null 2>&1; then
  echo "release tag preflight accepted a non-main branch" >&2
  exit 1
fi
git -C "$temporary/repository" switch -q main

if "$temporary/repository/scripts/release-tag.sh" stable >/dev/null 2>&1; then
  echo "stable tag was created without a candidate" >&2
  exit 1
fi

printf 'new release commit\n' >>"$temporary/repository/product"
git -C "$temporary/repository" add product
git -C "$temporary/repository" commit -q -m "advance release"
if "$temporary/repository/scripts/release-tag.sh" check >/dev/null 2>&1; then
  echo "release tag preflight accepted HEAD ahead of origin/main" >&2
  exit 1
fi
git -C "$temporary/repository" update-ref refs/remotes/origin/main HEAD

"$temporary/repository/scripts/release-tag.sh" candidate >/dev/null
test "$(git -C "$temporary/repository" rev-list -n 1 v1.0.0-rc.1)" = \
  "$(git -C "$temporary/repository" rev-parse HEAD)"

"$temporary/repository/scripts/release-tag.sh" stable >/dev/null
test "$(git -C "$temporary/repository" rev-list -n 1 v1.0.0)" = \
  "$(git -C "$temporary/repository" rev-parse HEAD)"

if "$temporary/repository/scripts/release-tag.sh" candidate >/dev/null 2>&1; then
  echo "release tag command moved or recreated an existing candidate" >&2
  exit 1
fi

printf 'dirty\n' >>"$temporary/repository/product"
if "$temporary/repository/scripts/release-tag.sh" check >/dev/null 2>&1; then
  echo "release tag preflight accepted a dirty worktree" >&2
  exit 1
fi

echo "Release tag safety verified."
