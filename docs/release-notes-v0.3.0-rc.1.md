# v0.3.0-rc.1

v0.3 adds the security boundary and narrowly governed multi-provider paths required before expanding
InferCrane's public surface.

## Added

- role-bounded service-account scopes with rotation, revocation, tenant isolation, and audit identity
- reference-only secret objects with an environment resolver
- hard-budgeted, privacy-acknowledged external health fallback
- a governed OpenRouter adapter with no request replay or silent duplication
- a narrow AWS EC2 BYOC adapter using role assumption, explicit private networking, immutable OCI
  identity, deterministic client tokens, ownership tags, reconciliation, and orphan inventory
- read-only `doctor --aws` validation and v0.3 integration capability evidence

## Upgrade

Database migrations 021–023 apply automatically and add service-account scopes, secret references,
and external target policies. Existing principals receive only their pre-v0.3 role permissions;
the migration never silently grants `manage_secrets` or `manage_external`. Create an explicitly
scoped service account for those new actions and rotate older credentials toward least privilege.

AWS is disabled unless the complete `INFERCRANE_AWS_*` configuration is present. Existing RunPod,
serverless, and existing-target deployments retain their previous lifecycle.

## Deferred evidence

Real AWS, OpenRouter billing, RunPod, and GPU-runtime qualification remains deferred to the consolidated
manual v1 gate. No production performance or provider-pricing claim is made by this candidate.
