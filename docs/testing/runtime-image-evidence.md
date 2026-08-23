# Runtime image transfer evidence

Captured on 2026-08-23 with `docker buildx imagetools inspect --raw` against the immutable OCI
references used or considered by InferCrane. Sizes are the sum of compressed Linux/amd64 layer
bytes; they are registry-transfer evidence, not extracted disk usage or startup duration.

| Runtime image | Linux/amd64 manifest | Compressed bytes | Layers | Status |
| --- | --- | ---: | ---: | --- |
| vLLM 0.22.0 production digest | `sha256:69cf768308bad3a6fde6ffeffc8ba1f28433752c01e9cb53f57bfaf547cec2e1` | 9,219,894,117 | 31 | Real AWS vLLM and portable custom-OCI protocol, benchmark, and delete paths passed at InferCrane `48d957d` |
| SGLang 0.5.12 default digest | `sha256:015f39a45844be5a7b35270c56dc4d9ebcfe9b0c21a3b4f877a4ee22e795bd7a` | 12,992,707,523 | 61 | Real AWS immutable-revision, protocol, benchmark, and delete path passed at InferCrane `48d957d` |
| SGLang 0.5.12 `runtime` candidate | `sha256:7de5f60ce864919b15af674de1f1b0223121ee42e83bb58f4f3aee16fb18ccfd` | 11,896,018,638 | 26 | Selected as an exact-digest release candidate; real AWS runtime qualification pending |

The SGLang runtime candidate is 1,096,688,885 compressed bytes (8.44%) smaller than the current
default for this exact version. That is useful but not enough to bypass runtime qualification. It
must pass immutable revision, readiness, model identity, buffered/streaming requests, benchmark,
and deletion on a real GPU before replacing the default.

The larger immediate improvement is avoiding transfer on a cache hit. AWS and GCP bootstrap now
inspect the exact digest locally before pulling and publish credential-free startup timestamps.
Model-weight caching remains a separate provider-native boundary and is never inferred from image
locality.

The corresponding real request measurements and their intentionally narrow methodology are recorded
in [AWS real-infrastructure evidence](./aws-real-evidence.md).
