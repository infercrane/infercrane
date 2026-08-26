#!/bin/sh
set -eu

root=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)
command_name=${1:-check}

usage() {
  cat >&2 <<'EOF'
usage: ./scripts/release-tag.sh check|candidate|stable

  check      verify that HEAD is safe to tag
  candidate  create the configured release-candidate tag locally
  stable     create the configured stable tag locally at the qualified RC commit

This command never pushes a tag or publishes a GitHub release.
EOF
  exit 2
}

case "$command_name" in
  check|candidate|stable) ;;
  *) usage ;;
esac

for required_command in git jq; do
  command -v "$required_command" >/dev/null 2>&1 || {
    echo "$required_command is required" >&2
    exit 1
  }
done

cd "$root"

version=$(jq -er '.version' .release/version.json)
candidate_tag=$(jq -er '.candidate_tag' .release/version.json)
stable_tag=$(jq -er '.stable_tag' .release/version.json)

printf '%s\n' "$version" | grep -Eq '^[0-9]+\.[0-9]+\.[0-9]+$' || {
  echo "release version must be semantic version X.Y.Z: $version" >&2
  exit 1
}
test "$stable_tag" = "v$version" || {
  echo "stable tag $stable_tag does not match version $version" >&2
  exit 1
}
version_pattern=$(printf '%s' "$version" | sed 's/\./\\./g')
printf '%s\n' "$candidate_tag" | grep -Eq "^v${version_pattern}-rc\.[1-9][0-9]*$" || {
  echo "candidate tag $candidate_tag must match v$version-rc.N" >&2
  exit 1
}

branch=$(git symbolic-ref --quiet --short HEAD 2>/dev/null || true)
test "$branch" = main || {
  echo "release tags must be created from main, not ${branch:-detached HEAD}" >&2
  exit 1
}

test -z "$(git status --porcelain=v1 --untracked-files=all)" || {
  echo "release tags require a clean worktree" >&2
  exit 1
}

head_sha=$(git rev-parse HEAD)
origin_main=$(git rev-parse --verify refs/remotes/origin/main 2>/dev/null || true)
test -n "$origin_main" || {
  echo "origin/main is unavailable; run: git fetch --prune origin" >&2
  exit 1
}
test "$head_sha" = "$origin_main" || {
  echo "HEAD does not match origin/main; fetch, review, and synchronize before tagging" >&2
  exit 1
}

if [ "$command_name" = check ]; then
  printf 'release tag preflight passed: version=%s candidate=%s stable=%s commit=%s\n' \
    "$version" "$candidate_tag" "$stable_tag" "$head_sha"
  exit 0
fi

tag=$candidate_tag
if [ "$command_name" = stable ]; then
  tag=$stable_tag
  git rev-parse --verify "refs/tags/$candidate_tag" >/dev/null 2>&1 || {
    echo "qualified candidate tag $candidate_tag does not exist locally" >&2
    exit 1
  }
  candidate_sha=$(git rev-list -n 1 "$candidate_tag")
  test "$candidate_sha" = "$head_sha" || {
    echo "stable tag must point to the exact qualified candidate commit" >&2
    echo "candidate: $candidate_sha" >&2
    echo "HEAD:      $head_sha" >&2
    exit 1
  }
fi

if git rev-parse --verify "refs/tags/$tag" >/dev/null 2>&1; then
  echo "tag already exists and will not be moved: $tag" >&2
  exit 1
fi

git tag -a "$tag" -m "InferCrane $tag" "$head_sha"
printf 'created local tag %s at %s\n' "$tag" "$head_sha"
printf 'not pushed; inspect it with: git show --stat %s\n' "$tag"
