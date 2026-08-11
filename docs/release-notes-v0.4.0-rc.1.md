# v0.4.0-rc.1

v0.4 makes InferCrane usable from delivery automation without changing lifecycle ownership.

## Added

- Executable OpenAPI 3.1 contract and route-drift qualification
- Generated low-level Python and TypeScript clients
- Ergonomic synchronous/asynchronous durable operation and SSE streaming helpers
- Terraform Plugin Framework provider for logical deployments
- GitHub Action for deterministic semantic plans, protected apply, and exact revision checks
- Interactive Mintlify control API pages

## Safety boundaries

- Client timeouts do not cancel server operations.
- Streaming helpers never replay a request after transmission starts.
- Terraform does not own provider replicas or bypass Release Guard.
- GitHub pull-request plan mode cannot mutate deployments.
- Packages, action tags, and Terraform Registry artifacts are not published by this local RC.

Paid provider qualification remains deferred to the consolidated v1.0 manual acceptance run.
