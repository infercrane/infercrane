# CLI reference

Public workflows use the authenticated control-plane API and support `--output json` where output is returned.

- `init`: create a private client configuration.
- `doctor`: validate dependencies; add `--cloud` or `--serverless` for provider checks.
- `deploy`, `apply`, `plan`: create, converge, or preview a DeploymentSpec.
- `status [--watch]`, `events`, `inspect`: view health, durable events, or raw infrastructure details.
- `rollout`: create, provision, evaluate, promote, reject, rollback, or inspect revisions.
- `benchmark`: run AIPerf and persist reproduction metadata.
- `explain`, `explain scaling|rollout|cold-start`: reproduce operational decisions.
- `delete`: withdraw routing and durably clean provider resources.
- `operation`: inspect or request cancellation of a durable operation.
- `version`: print the build version.

Mutations accept idempotency keys. Ctrl-C only disconnects the client or requests subprocess cancellation; it does not rewrite persisted lifecycle state.
