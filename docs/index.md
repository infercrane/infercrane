# Engineering knowledge index

This is the authoritative router for project knowledge. Read documents by task relevance rather
than loading the entire repository into one context window.

## Orientation

- [Quickstart](quickstart.md)
- [Concepts](concepts.md)
- [DeploymentSpec](deployment-spec.md)
- [CLI reference](cli.md)
- [Product vision and roadmap](product-vision.md): product promise, golden path, milestones, and success measures.
- [Enterprise readiness plan](enterprise-readiness.md): prioritized blockers and qualification order.
- [Control-plane API v1](control-api.md): authentication, roles, resources, and stable errors.
- [Project status](project-status.md): what is implemented, experimental, or planned.
- [Generated repository map](generated/repository-map.md): packages, commands, endpoints,
  configuration, migrations, and tests derived from source.
- [Ownership](ownership.md): stewardship and review responsibilities.
- [Contributing](https://github.com/infercrane/infercrane/blob/main/CONTRIBUTING.md): development and review workflow.

## Architecture

- [System architecture](architecture/system.md)
- [Data flows](architecture/data-flows.md)
- [System invariants](architecture/invariants.md)
- [Architecture decisions](adr/README.md)

## Features

- [Gateway and request data plane](features/gateway.md)
- [Persistence and migrations](features/persistence.md)
- [Reconciliation and routing](features/reconciliation.md)
- [Provisioning and runtimes](features/provisioning.md)
- [Configuration and operations](features/operations.md)
- [RunPod Serverless](features/serverless.md)
- [Cold-start intelligence](features/cold-starts.md)
- [Reproducible benchmarking](features/benchmarking.md)
- [Deterministic explanations](features/explanations.md)
- [Lifecycle](features/lifecycle.md)
- [Autoscaling](features/autoscaling.md)
- [Release Guard](features/release-guard.md)
- [Durable Sessions](features/durable-sessions.md)

## Operations

- [Production operations](production.md)
- [Compatibility and qualification](compatibility.md)
- [Backup and restore runbook](runbooks/backup-restore.md)
- [Stage 1 existing-worker guide](stage1-poc.md)
- [Stage 2 SkyPilot guide](stage2-skypilot.md)
- [Dependencies](dependencies.md)
- [RunPod provider setup](provider-setup.md)
- [Troubleshooting](troubleshooting.md)
- [Security](security.md)
- [FAQ](faq.md)

## Authority order

When sources disagree, resolve the conflict rather than choosing silently:

1. Tests, migrations, API behavior, and executable contracts.
2. Accepted ADRs and system invariants.
3. Feature and operations documentation.
4. Generated repository inventory.
5. README, issues, pull requests, and chat history.

The generated map is factual navigation, not design authority. Chat history is never durable
project memory.
