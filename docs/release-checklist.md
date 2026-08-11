# v1.0 release-candidate checklist

An RC tag proves the automated implementation gates only. Real provider evidence is gathered once,
after `v1.0.0-rc.1`, before any stable tag or public release. Never convert a deferred row to passed
without its durable log and direct inventory evidence.

## Automated RC tag

- [ ] `make context`, `make verify`, `make deadcode`, and `make audit` pass from one clean commit.
- [ ] Every migration prefix upgrades; concurrent startup is serialized; modified, gapped, and
      newer migration ledgers fail closed.
- [ ] Provider/runtime contract, fault-injection, Docker/PostgreSQL, restart, Kind, SDK, Terraform,
      GitHub Action, dashboard, and documentation gates pass.
- [ ] The provider-neutral production image contains no development fakes and executes its pinned
      AWS CLI v2 and `kubectl` boundaries on amd64 and arm64.
- [ ] `make candidate-artifacts RELEASE_CANDIDATE_TAG=v1.0.0-rc.1` produces exactly four archives,
      checksums, SPDX SBOMs, a generated Homebrew formula, and a native install smoke test.
- [ ] Release notes, compatibility, upgrade, security, production, and provider/runtime docs match
      executable capability states.
- [ ] `.release/evidence/v1.0.0-rc.1.md` records exact commands and artifacts.
- [ ] The worktree is clean and local annotated tag `v1.0.0-rc.1` points at the evidence commit.

## Consolidated manual qualification

- [ ] Read-only provider preflight succeeds before mutation.
- [ ] RunPod elastic/serverless and disruption suites pass, including disconnect, restart,
      create-response loss, stream cancellation, generation-safe drain, rollback, and zero inventory.
- [ ] AWS BYOC runs real vLLM, SGLang, and custom OCI specs through readiness, buffered/streaming
      requests, benchmark, delete, reconciliation, and direct zero-billable-instance inventory.
- [ ] Kubernetes runs the same runtime matrix on a real GPU cluster and directly proves zero
      InferCrane-managed workloads and services afterward.
- [ ] Hermetic governed-fallback evidence remains attached; any real external API test is explicit,
      privacy-acknowledged, hard-budgeted, and never silently duplicated.
- [ ] Real benchmark/passport evidence records exact model commit, image digest, runtime, GPU,
      provider, region, workload, errors, and trustworthy cost metadata only.
- [ ] `scripts/v1-acceptance.sh report` reports `real_infrastructure: passed` for the same commit.
- [ ] An operator independently confirms RunPod run-owned Pods/endpoints are zero and the AWS and
      Kubernetes managed inventories match their pre-run baselines.

## Publication

- [ ] Upload archives, checksums, SBOMs, image digest, image SBOM, and provenance to a draft release.
- [ ] Substitute the real release URL into the generated Homebrew formula and perform clean-machine
      install, completion, `doctor`, and `version` checks.
- [ ] Review image/archive vulnerability results and sanitize evidence for credentials or content.
- [ ] Publish no performance or cost comparison without reproducible attached evidence.
- [ ] Create stable tag `v1.0.0` only after every manual row is proven.
- [ ] Follow the exact stable artifact, tag, push, and `gh release create` sequence in
      [Release packaging](/release-packaging); publication requires separate authorization.
