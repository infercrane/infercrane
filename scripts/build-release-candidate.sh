#!/bin/sh
set -eu

root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
tag=${1:-v1.0.0-rc.1}
goreleaser_version=${2:-v2.12.7}

case "$tag" in v[0-9]*.[0-9]*.[0-9]*-rc.[0-9]*) ;; *) echo "invalid release-candidate tag: $tag" >&2; exit 2;; esac
[ -z "$(git -C "$root" status --porcelain)" ] || {
  echo "release-candidate artifacts require a clean worktree" >&2
  exit 1
}

# GoReleaser requires the requested release tag to exist at HEAD. The RC tag is
# deliberately created only after every artifact gate passes, so build inside
# an isolated local clone with a temporary annotated tag. No source ref is
# added, removed, pushed, or published by this operation.
build_root=$(mktemp -d)
trap 'rm -rf "$build_root"' EXIT HUP INT TERM
git clone --quiet --local --no-hardlinks "$root" "$build_root/source"
git -C "$build_root/source" tag -a "$tag" -m "temporary isolated build tag $tag" HEAD
(
  cd "$build_root/source"
  go run "github.com/goreleaser/goreleaser/v2@$goreleaser_version" release --clean --skip=announce --skip=publish
)

rm -rf "$root/dist"
mkdir -p "$root/dist"
cp -R "$build_root/source/dist/." "$root/dist/"
