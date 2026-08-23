package provision

// cachedImageBootstrap reuses an immutable image already present in a
// customer-prewarmed machine image. It deliberately does not infer that model
// artifacts are cached: container and artifact locality are separate evidence
// boundaries.
func cachedImageBootstrap(image string) string {
	return cachedImageBootstrapWithPolicy(image, "prefer")
}

func cachedImageBootstrapWithPolicy(image, policy string) string {
	quoted := shellQuote(image)
	if policy == "required" {
		return "infercrane_stage image_check\nif docker image inspect " + quoted + " >/dev/null 2>&1; then\n  infercrane_stage image_cache_hit\nelse\n  infercrane_stage image_cache_miss_required\n  echo 'required immutable runtime image is not prewarmed' >&2\n  exit 1\nfi\n"
	}
	return "infercrane_stage image_check\nif docker image inspect " + quoted + " >/dev/null 2>&1; then\n  infercrane_stage image_cache_hit\nelse\n  infercrane_stage image_pull_start\n  docker pull " + quoted + "\n  infercrane_stage image_pull_complete\nfi\n"
}
