#!/usr/bin/env sh
set -eu

range=${1:-}
if [ -z "$range" ]; then
  echo "usage: scripts/check-dco.sh BASE..HEAD" >&2
  exit 2
fi

missing=""
for commit in $(git rev-list --no-merges "$range"); do
  author=$(git show -s --format='%an <%ae>' "$commit")
  if ! git show -s --format='%B' "$commit" | grep -Fqi "Signed-off-by: $author"; then
    missing="$missing\n  $commit  $author"
  fi
done

if [ -n "$missing" ]; then
  printf 'Commits missing an author-matching DCO sign-off:%b\n' "$missing" >&2
  echo "Amend each commit with: git commit --amend --signoff" >&2
  exit 1
fi

echo "DCO sign-offs verified for $range"
