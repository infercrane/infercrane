# ADR 0011: Tier provider testing and isolate paid qualification

- Status: Accepted
- Date: 2026-08-11

## Context

InferCrane's first provider qualification exposed control-plane, harness, template, image, capacity,
and provider-host failures through the same long paid workflow. Correct lifecycle code still took
minutes to distinguish from external allocation and image-transfer behavior. Repeating the entire
workflow made feedback slow, spent provider credits, and risked duplicate operator actions.

Infrastructure ecosystems separate fast contract tests from real acceptance. Terraform's
[acceptance guidance](https://developer.hashicorp.com/terraform/plugin/testing/acceptance-tests)
requires an explicit opt-in, recommends dedicated test accounts, verifies destruction, and runs
real provider acceptance on a scheduled cadence. Its testing framework also provides
[destroy checks](https://developer.hashicorp.com/terraform/plugin/testing/acceptance-tests/testcase)
and [sweepers](https://developer.hashicorp.com/terraform/plugin/testing/acceptance-tests/sweepers)
for leaked resources. InferCrane needs the same separation while retaining end-to-end release
evidence.

## Decision

InferCrane uses four qualification tiers:

1. Standard Go tests prove deterministic state, policy, and adapter behavior.
2. Provider contract tests prove replay-safe create/adopt/observe/delete semantics against hermetic
   protocol fixtures, including lost responses and failure transitions.
3. Isolated Docker tests prove the real control-plane process, PostgreSQL, router, gateway, CLI,
   streaming, interruption, and restart behavior without cloud credentials.
4. Explicit paid tests qualify a registered adapter against real infrastructure for a frozen
   candidate. They use stable run ownership, a single paid-run lock, bounded resources and time,
   sanitized evidence, guarded cleanup, and direct post-delete inventory.

Registration and qualification remain separate. A fake provider is test infrastructure, never a
production backend or support claim. CI runs tiers one through three. Paid provider canaries and
release qualification remain credentialed workflows with explicit authorization.

## Consequences

Most defects fail in seconds or minutes. Provider outages and runtime delivery failures are isolated
from control-plane regressions. Adding a provider requires implementing the existing contracts and
passing identical lifecycle scenarios before real-cloud qualification.

The emulator and fixtures must track provider schemas and redact secrets. Hermetic success cannot
prove stock, billing, GPU drivers, image distribution, model transfer, or provider timing; those
remain real-infrastructure gates.

## Alternatives

Running every test against real clouds was rejected because it is slow, costly, nondeterministic,
and unsafe for ordinary pull requests. Mocking workflow internals was rejected because it bypasses
adapter protocol and replay behavior. Kubernetes-based test infrastructure was rejected because
InferCrane v0.1 neither uses nor requires Kubernetes.

## Verification

`make dev-check` runs repository and provider contract checks. `make dev-check-full` runs the
isolated Docker qualification. CI runs the full local command and retains evidence. Paid acceptance
refuses concurrent mutation and verifies direct provider inventory after cleanup.
