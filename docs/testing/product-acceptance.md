# Product acceptance

`scripts/product-acceptance.sh` qualifies InferCrane through its public CLI, HTTP gateway, dashboard,
SDK examples, and durable control-plane behavior. It complements package tests: the runner starts a
clean stack and performs user journeys without reading or mutating PostgreSQL directly.

## Local journeys

```bash
export INFERCRANE_PRODUCT_ACCEPTANCE_RUN_ID="$(date -u +%Y%m%dT%H%M%SZ)-$(git rev-parse --short HEAD)"
./scripts/product-acceptance.sh local
```

The fixed run ID keeps evidence together under `.infercrane/product-acceptance/RUN_ID`. Individual
journeys are available when investigating a failure:

```bash
./scripts/product-acceptance.sh list
./scripts/product-acceptance.sh offline
./scripts/product-acceptance.sh first-value
./scripts/product-acceptance.sh adoption
./scripts/product-acceptance.sh reliability
./scripts/product-acceptance.sh release
./scripts/product-acceptance.sh report
```

The journeys prove:

- native binary startup, offline initialization, private context storage, completions, and signing-key permissions;
- first request through buffered, streaming, tools, structured output, completions, and embeddings;
- Python and TypeScript client compatibility;
- dashboard security headers and authenticated control API behavior;
- status, events, inspect, integrations, explanations, and JSON automation output;
- observe-only and traffic-managed adoption without deleting externally owned workers;
- control-plane failure recovery, HA, and backup/restore behavior;
- the full repository, provider-contract, Kind, container, documentation, audit, dead-code, and
  release-package gates.

Local fake runtimes prove InferCrane behavior, not GPU model quality or provider semantics. Real
RunPod, AWS, and Kubernetes qualification remains separate and explicitly paid. Use the consolidated
RunPod orchestrator in `docs/v2-manual-qualification.md`; never run it concurrently with another paid
acceptance command.

## Acceptance rules

- Run from a clean worktree when producing reusable release evidence.
- Keep at least 15 GiB free disk; 30 GiB is recommended for container builds.
- A failed journey retains its complete log and stops the sequence.
- Fix a product failure and add a focused regression before rerunning the journey.
- Never interpret local fixtures as real-provider qualification.
