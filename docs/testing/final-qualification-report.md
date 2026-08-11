# InferCrane whole-product qualification report

Date: 2026-08-11  
Qualified implementation commit: `c22c726b4ca96097511dba3f53a4bedcea830145`  
Verdict: **AUTOMATED QUALIFICATION COMPLETE — MANUAL INFRASTRUCTURE QUALIFICATION REMAINS**

## Scope and result

The inventory contains 79 product and operational surfaces. The strongest safe local boundary is
green for all of them: 58 are fully `AUTOMATED_QUALIFIED`, 16 remain
`REAL_INFRA_REQUIRED`, and five require a human/hosted-system review. A fixture, Kind cluster, mocked
provider, or packaging smoke is never recorded as real GPU/provider evidence.

The authoritative row-by-row result is
[feature qualification matrix](/testing/feature-qualification-matrix), and the properties those tests
prove are in [invariants](/testing/invariants).

## Defects found and fixed

1. Persistent Context Passports were never connected to the production gateway. The server now loads
   and refreshes an atomic snapshot, publishes newly created hints immediately, removes stale hints,
   and falls back to ordinary healthy routing.
2. FinOps could accept future observations, use evidence outside the requested report window, and
   default missing currency to USD. It now rejects untrustworthy evidence and preserves an explicit
   `unavailable` result without inventing currency or savings.
3. Retrying an identical Autopilot plan could conflict because PostgreSQL JSONB formatting was compared
   as raw text. Candidate and evidence JSON are now compared semantically.
4. `infercrane capacity --window` sent the window as an idempotency header, so the API used its default
   window. It now sends the exact query parameter and has route-level CLI coverage.
5. Intelligence commands inconsistently accepted unsupported output formats. Replay, Capacity,
   FinOps, Autopilot, Session, and Burst Guard now consistently accept only `human` or `json`.
6. Repeated `make test-container` runs reused PostgreSQL test state and failed with fixture conflicts.
   The target now recreates only its two test-profile containers; two consecutive runs passed.
7. Release metadata had drifted: the v2 binary/OpenAPI train still packaged v1 SDKs and defaults. The
   binary, Python/TypeScript SDKs and User-Agents, GitHub Action, production image, release scripts,
   and current release docs now agree on `v2.0.0-rc.1`; automation enforces parity.

## Automated evidence

- `./scripts/dev-check.sh full` passed. Durable evidence:
  `.infercrane/dev-check/20260811T181856Z-4268`.
- `make test-container` passed twice consecutively after the isolation correction. Both executions ran
  `go test -race -count=1 ./...` against PostgreSQL.
- `make verify` passed: formatting, module integrity, repository/OpenAPI drift, race suite, client
  automation, dashboard, vet, build, and Compose validation.
- Kind/manifests, provider/runtime contracts, stack smoke, failure recovery, control-plane HA,
  backup/restore, production Compose, SDKs, Terraform, GitHub Action, dashboard, and acceptance safety
  passed through the full developer gate.
- `make audit` reported no reachable Go vulnerabilities in the root or Terraform modules and no high
  severity TypeScript dependency vulnerabilities.
- `make deadcode release-check` passed.
- `INFERCRANE_ACCEPTANCE_RUN_ID=20260811T183137Z-automated-local make acceptance-local` passed and
  removed its local stack.
- Commit-bound integration evidence passed at
  `.infercrane/contract-qualification/c22c726b4ca96097511dba3f53a4bedcea830145/qualification.json`.
- `make candidate-artifacts` built and verified exactly four `v2.0.0-rc.1` archives, checksums, SPDX
  SBOMs, a native CLI smoke, and a Homebrew formula. Nothing was published.
- Mintlify validation, links, anchors, snippets, and 151-page accessibility scan passed. The blue
  primary is WCAG AA on light backgrounds but Mintlify recommends considering AAA.

## Real infrastructure still required

### RunPod elastic and serverless

This is the supported resumable paid qualification for real vLLM, streaming/tool/structured output,
AIPerf, autoscaling, Release Guard, cold/warm/zero, cancellation, disruption, adoption, deletion, and
direct zero-inventory evidence:

```bash
export RUNPOD_KEY_FILE="$HOME/.config/infercrane/runpod-key"
export INFERCRANE_RUNPOD_SERVERLESS_TEMPLATE_ID="REPLACE_WITH_IMMUTABLE_TEMPLATE_ID"
export INFERCRANE_V2_QUALIFICATION_RUN_ID="$(date -u +%Y%m%dT%H%M%SZ)-v2.0.0-rc.1"

./scripts/qualify-v2-manual.sh run --approve-paid-resources
./scripts/qualify-v2-manual.sh status
./scripts/qualify-v2-manual.sh cleanup
```

Reuse the same run ID after a disconnect. After cleanup, independently confirm zero run-owned RunPod
Pods and Serverless endpoints.

### AWS and real Kubernetes GPU

Prepare the private env/spec/key inputs in [release-acceptance.md](../release-acceptance.md), build or
load the exact v2 candidate image, and run each provider separately:

```bash
export INFERCRANE_ACCEPTANCE_RUN_ID="$(date -u +%Y%m%dT%H%M%SZ)-v2-aws"
export INFERCRANE_V1_PROVIDER_ENV_FILE=/private/infercrane-aws.env
export INFERCRANE_V1_SPEC_DIR=/private/infercrane-v2-specs/aws
export INFERCRANE_V1_API_KEY_FILE=/private/infercrane-worker-and-control-key
export INFERCRANE_V1_IMAGE=infercrane:v2.0.0-rc.1
./scripts/portable-provider-acceptance.sh aws --approve-paid-resources

export INFERCRANE_ACCEPTANCE_RUN_ID="$(date -u +%Y%m%dT%H%M%SZ)-v2-kubernetes"
export INFERCRANE_V1_PROVIDER_ENV_FILE=/private/infercrane-kubernetes.env
export INFERCRANE_V1_SPEC_DIR=/private/infercrane-v2-specs/kubernetes
export INFERCRANE_V1_IMAGE=infercrane:v2.0.0-rc.1
./scripts/portable-provider-acceptance.sh kubernetes --approve-paid-resources
```

The `INFERCRANE_V1_*` names are retained script compatibility names, not a v1 artifact requirement.
Confirm AWS/Kubernetes managed inventory exactly matches its recorded pre-run baseline.

### Other registered boundaries

GCP Compute is hermetically local-qualified, while ASG, Bedrock, EKS, SageMaker, GKE, MIG, Vertex, and
CoreWeave CKS are registered contract boundaries. There is no repository-owned consolidated real
qualification harness for those boundaries yet. They must remain advertised as unqualified/deferred,
not as fully supported production adapters. Real SGLang, custom OCI, Dynamo request survival,
provider-native cache/prefetch, external managed fallback, trustworthy cost/pricing, and real AIPerf
also remain external evidence requirements.

## Human and hosted-system checks

- Review terminal UI at narrow/normal/wide PTY sizes: keyboard navigation, action confirmation,
  empty/loading/error states, copy/resume, and screen-reader-friendly text.
- Review the browser dashboard on mobile/desktop and light/dark modes, including authentication,
  loading, empty, stale, and failure states.
- Confirm the GitHub Action in hosted Actions without applying unless an environment approval is set.
- Confirm the deployed Mintlify site at the configured production domain after Git sync.
- Inspect paid acceptance reports and direct provider inventory; local rows alone never prove deletion.

## Known limitations

- No paid provider, real GPU, real runtime, external API, or customer network was used in this goal.
- Context Passport is best-effort logical affinity, not durable KV. The in-memory snapshot is bounded
  to 10,000 active hints per control-plane instance; reliability falls back to normal healthy routing.
- Cost, price, savings, cache state, provider timing, and hidden cold-start boundaries remain unavailable
  unless sourced evidence exists.
- Registered/deferred provider profiles are not equivalent to executable, real-qualified adapters.
- Mintlify's primary blue meets WCAG AA but not AAA on the light background.

## Release boundary

Do not create or move a tag, push, publish packages/images, or claim production provider support until
the applicable real/manual rows above are attached to this exact implementation lineage. The automated
implementation is qualified; the public production release is not yet fully manually qualified.
