# Release sequence

Milestones are implemented sequentially on the current branch. An intermediate milestone receives
an annotated `-rc.1` tag only after its automated gates pass on a clean tree. Real paid-provider
qualification is deferred until the consolidated v1.0 release-candidate exercise, so these tags are
engineering checkpoints rather than public stable releases.

| Milestone | State | Outcome |
|---|---|---|
| v0.2.0 | Automated gates passed | Provider Contract V1, Runtime Contract V1, capabilities and conformance |
| v0.3.0 | Automated gates passed | Security foundation, governed external targets, OpenRouter and narrow AWS BYOC |
| v0.4.0 | Automated gates passed | Python/TypeScript SDKs, Terraform and GitHub integration |
| v0.5.0 | Automated gates passed | Operational dashboard and fleet evidence |
| v0.6.0 | Automated gates passed | Qualified OCI workloads and SGLang |
| v0.7.0 | Automated gates passed | Recommendations, capacity intelligence, hybrid overflow and SLO policy |
| v0.8.0 | Planned | Release Guard V2 and Inference Passport |
| v0.9.0 | Planned | Kubernetes/KServe and conditional advanced runtime integrations |
| v1.0.0 | Planned | Multi-provider hardening, packaging and consolidated RC evidence |

Each milestone specification defines scope, exclusions, public surface, verification, and evidence.
The machine-readable current state is `.release/current.json`; gates are `.release/gates.yaml`.

## Release loop

1. Refine acceptance criteria before implementation.
2. Implement the smallest coherent change without weakening architectural boundaries.
3. Add unit, integration, contract, fault, migration, security, CLI/API, and regression coverage.
4. Update public docs and generated inventories in the same milestone.
5. Run focused checks during development and complete local/Docker qualification at the boundary.
6. Record evidence, review the diff, commit, create the local annotated RC tag, and continue.

No stable tag, package publication, remote tag push, or paid-resource creation is authorized by this
sequence.
