# CLI reference

Public workflows use the authenticated control-plane API and support `--output json` where output is returned.

- `init`: create a private client configuration.
- `doctor`: ask the control plane to validate its dependencies; add `--cloud` or `--serverless` for provider checks.
- `deploy`, `apply`, `plan`: create, converge, or preview a DeploymentSpec.
- `status [--watch]`, `events`, `inspect`: view health, durable events, or raw infrastructure details.
- `rollout`: create, provision, evaluate, promote, reject, rollback, or inspect revisions.
- `benchmark`: run AIPerf and persist reproduction metadata.
- `explain`, `explain scaling|rollout|cold-start`: reproduce operational decisions.
- `delete`: withdraw routing and durably clean provider resources.
- `operation`: inspect or request cancellation of a durable operation.
- `version`: print the build version.

Mutations accept idempotency keys. Ctrl-C only disconnects the client or requests subprocess cancellation; it does not rewrite persisted lifecycle state.
With `--wait --output json`, mutation commands print one final JSON document after polling durable
operation state. Human output may print intermediate durable progress. `delete --plan` is
side-effect-free and supports both output formats.
`status --watch --output json` emits one complete JSON document per line for each durable-state
refresh until the client disconnects.
