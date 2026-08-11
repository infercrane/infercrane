# InferCrane v1.5.0-rc.1

v1.5 proves the provider-neutral lifecycle with an independently identified GCP Compute path and
prevents cloud-product semantics from collapsing into one ambiguous provider switch.

## Highlights

- Immutable revisions can persist an exact `provider_adapter`; old unambiguous deployments continue
  to work while ambiguous composition fails closed.
- `gcp-compute` launches a private VM per replica intent using deterministic adoption identity,
  attached service-account credentials, immutable images, owned inventory, and idempotent cleanup.
- AWS ASG/EKS/SageMaker/Bedrock, GCP MIG/GKE/Vertex, and CoreWeave CKS are separate inspectable
  profiles with honest registered/deferred states.
- CLI, API, Python SDK, TypeScript SDK, examples, and docs expose exact advanced adapter selection.

## Qualification

All local RC and contract gates passed at
`1844c39be1f3ca220939d3b8b7a0bab44de94ed7`. No paid provider was contacted. Real provider and GPU
runtime qualification remains deferred and the integration inventory is authoritative.

## Known limitations

- The production image does not bundle Google Cloud CLI; a GCP-enabled control-plane host must
  provide an authenticated `gcloud` executable. Container packaging for that optional client remains
  deferred until its pinned supply-chain boundary is qualified.
- Managed/group profiles are registered boundaries, not executable adapters.
- GCP cost is `unknown`; InferCrane does not fabricate pricing.
