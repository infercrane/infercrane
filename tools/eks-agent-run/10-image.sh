#!/usr/bin/env bash
# Phase 1. Builds the InferCrane image from this checkout and pushes it to a
# private ECR repository, so the cluster's nodes can pull it with their own
# role. The published image (ghcr.io/infercrane/infercrane:v2.0.2, public
# since the release) is older than the code this run qualifies, and nothing
# of this test is ever pushed to a public registry.
#
# Creates in AWS: one private ECR repository, "infercrane-eks-proof".
# Cost: image storage, $0.10 per GB a month (under one cent for this test).
# On this laptop: one `docker build` for linux/arm64, the laptop's own
# architecture, so nothing is emulated. Run it only while no other session's
# campaign is using the laptop. No build when this laptop still holds an image
# this phase built for an earlier commit (for any region's repository) and
# nothing it is built from changed since: that image is tagged for this
# region's repository and pushed as it is, with the same digest.
#
# Skip this phase by naming an image the nodes can already pull:
#   EKS_PROOF_IMAGE=ghcr.io/infercrane/infercrane:v2.0.2
#
# Without --yes it prints what it would do and creates nothing.
. "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

YES=no
[ "${1:-}" = "--yes" ] && YES=yes

image_phase() {
  if [ -n "${EKS_PROOF_IMAGE:-}" ]; then
    say "EKS_PROOF_IMAGE is set to $EKS_PROOF_IMAGE: nothing to build, nothing to create."
    return 0
  fi
  need_tools aws docker git

  local commit dirty registry tag
  commit=$(git -C "$REPO_DIR" rev-parse --short=12 HEAD)
  # The image must be the committed code. Files of this tool and evidence
  # files are not part of the image's programs.
  dirty=$(git -C "$REPO_DIR" status --porcelain -- . ':!tools/eks-agent-run' ':!docs/testing/evidence' | head -5)
  registry=$(ecr_registry)
  tag="$registry/$ECR_REPOSITORY:$commit"

  # An image already pushed for an earlier commit is reused when nothing the
  # image is built from has changed since: only this tool, evidence or docs.
  # launch.sh calls this phase after every grant; on 26 Nov the image of
  # 72f08b29 was pushed before the tool's later commits.
  # It must also be in this region's registry; the first image of 26 Nov
  # recorded no region and was pushed to eu-central-1.
  local recorded pushed_to
  recorded=$(state_get IMAGE_COMMIT)
  pushed_to=$(state_get IMAGE_REGION)
  pushed_to=${pushed_to:-eu-central-1}
  if [ -n "$recorded" ] && [ -n "$(state_get IMAGE_DIGEST)" ] && [ "$pushed_to" = "$REGION" ] &&
    git -C "$REPO_DIR" diff --quiet "$recorded" HEAD -- . ':!tools/eks-agent-run' ':!docs' &&
    [ -z "$dirty" ] &&
    aws ecr describe-images --repository-name "$ECR_REPOSITORY" --image-ids "imageDigest=$(state_get IMAGE_DIGEST)" >/dev/null 2>&1; then
    say "The image of $recorded ($(state_get IMAGE_DIGEST)) is in $ECR_REPOSITORY, and nothing outside"
    say "tools/eks-agent-run and docs changed since; it is reused. Nothing is built or pushed."
    return 0
  fi
  # An image built here earlier, for this or another region's repository: on
  # 26 Nov the image of 72f08b29 was built for eu-central-1, whose repository
  # was deleted at 19:00 when Frankfurt had no L4; us-east-1 needs only a push.
  local source="" candidate built
  if [ -z "$dirty" ]; then
    while read -r candidate; do
      built=${candidate##*:}
      git -C "$REPO_DIR" rev-parse --verify --quiet "$built^{commit}" >/dev/null || continue
      git -C "$REPO_DIR" merge-base --is-ancestor "$built" HEAD || continue
      git -C "$REPO_DIR" diff --quiet "$built" HEAD -- . ':!tools/eks-agent-run' ':!docs' || continue
      source=$candidate
      commit=$built
      tag="$registry/$ECR_REPOSITORY:$commit"
      break
    done < <(docker image ls --format '{{.Repository}}:{{.Tag}}' | grep -E "^[0-9]+\.dkr\.ecr\.[a-z0-9-]+\.amazonaws\.com/$ECR_REPOSITORY:[0-9a-f]{12}\$" | sort -u || true)
  fi

  step "What this phase does"
  if [ -n "$source" ]; then
    say "  Reuses   $source ($(docker image inspect --format '{{.Id}}' "$source" | cut -c1-19)), built here from commit"
    say "           $commit; nothing outside tools/eks-agent-run and docs changed since. No build."
  else
    say "  Builds   the Dockerfile's runtime target for linux/arm64 from commit $commit"
  fi
  say "  Creates  ECR repository $ECR_REPOSITORY in $REGION (private, tagged project=$PROJECT_TAG)"
  say "  Pushes   $tag"
  say "  Cost     \$$(calc "$ECR_IMAGE_GB * $PRICE_ECR_GB_MONTH / $HOURS_PER_MONTH") an hour of storage, until 90-delete.sh removes the repository"
  say "  Differs from origin/main in these files outside tools/ and docs/testing/evidence:"
  git -C "$REPO_DIR" diff --stat origin/main...HEAD -- . ':!tools/eks-agent-run' ':!docs/testing/evidence' | sed 's/^/    /'
  [ -z "$dirty" ] || die "the checkout has uncommitted changes outside this tool; commit them first:
$dirty"
  if [ "$YES" != yes ]; then
    say ""
    say "Nothing was built or created. Run again with --yes to do it."
    return 0
  fi

  step "Creating the ECR repository"
  if aws ecr describe-repositories --repository-names "$ECR_REPOSITORY" >/dev/null 2>&1; then
    say "  it exists already"
  else
    aws ecr create-repository --repository-name "$ECR_REPOSITORY" --image-tag-mutability IMMUTABLE \
      --tags "Key=project,Value=$PROJECT_TAG" --query 'repository.repositoryName' --output text
  fi

  if [ -n "$source" ]; then
    step "Tagging $source as $tag (no build)"
    [ "$source" = "$tag" ] || docker tag "$source" "$tag"
  else
    step "Building $tag"
    docker build --platform linux/arm64 --target runtime --build-arg "INFERCRANE_VERSION=eks-proof-$commit" -t "$tag" "$REPO_DIR"
  fi

  step "Pushing"
  # The registry password goes into a throwaway Docker config that is removed
  # when this script ends; it is never printed and never kept.
  local docker_host config
  docker_host=$(docker context inspect --format '{{.Endpoints.docker.Host}}')
  config=$(mktemp -d)
  # shellcheck disable=SC2064
  trap "rm -rf '$config'" EXIT
  if aws ecr describe-images --repository-name "$ECR_REPOSITORY" --image-ids "imageTag=$commit" >/dev/null 2>&1; then
    say "  tag $commit is in the repository already"
  else
    aws ecr get-login-password | DOCKER_HOST="$docker_host" docker --config "$config" login --username AWS --password-stdin "$registry" >/dev/null 2>&1
    DOCKER_HOST="$docker_host" docker --config "$config" push "$tag"
  fi
  rm -rf "$config"

  local digest
  digest=$(aws ecr describe-images --repository-name "$ECR_REPOSITORY" --image-ids "imageTag=$commit" --query 'imageDetails[0].imageDigest' --output text)
  case "$digest" in sha256:*) ;; *) die "the pushed image has no digest" ;; esac
  state_set IMAGE_COMMIT "$commit"
  state_set IMAGE_DIGEST "$digest"
  state_set IMAGE_REGION "$REGION"
  say ""
  say "Image ready: $registry/$ECR_REPOSITORY@$digest (commit $commit)."
}

run_masked image_phase
