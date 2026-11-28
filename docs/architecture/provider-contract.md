---
title: Provider contract
description: The lifecycle, ownership, idempotency, inventory, and qualification contract for infrastructure adapters.
---

# Provider Contract V1

Status: implemented contract foundation. Existing elastic and serverless adapters are
bound to validated profiles without changing durable lifecycle semantics; real-provider evidence
remains deferred until consolidated real-infrastructure qualification.

## Ownership

The lifecycle core owns desired intent, serialized mutations, retry policy, revisions and durable
evidence. A provider adapter owns translation to one external infrastructure API. Provider-native
serverless owns worker scheduling; InferCrane owns the logical endpoint and operation history.

## Required lifecycle semantics

- validate configuration and report capabilities without mutation
- derive a stable external idempotency/adoption key from persisted replica intent
- ensure or adopt exactly one external resource
- observe normalized phase, endpoint, health and provider identity
- cancel an in-progress request when supported without abandoning cleanup
- delete idempotently and prove absence through inventory
- discover owned or orphaned resources using explicit ownership metadata
- bound calls, redact credentials, classify retryability and preserve provider-native details

Elastic, serverless and external targets share capability vocabulary but do not pretend to have the
same lifecycle. External targets are registered endpoints; InferCrane does not provision them.

## Qualification states

`registered`, `simulated`, `local-qualified`, `real-qualified`, `deferred`, and `failed` are distinct.
A capability or adapter registration never implies public support. Evidence is tied to contract
version, adapter version, commit, test suite, environment class, timestamp and sanitized artifacts.

A runtime compatibility entry covers a runtime on one adapter and compute mode. A real-cluster run
that qualified one exact tuple adds a narrowed entry beside the general one: the GPUs
(`accelerators`, compared without vendor prefix, case, or separators), the engine version
(`runtime_version`), and the pinned image (`image`), with its evidence. Only a `local-qualified` or
`real-qualified` entry may be narrowed. Where several entries apply to a request, the planner and the
evidence rows take the best-qualified one, and the narrowed one on a tie; every other GPU and
version keeps the general entry's state.

## Prerequisites

A provider profile may name a `prerequisite`: software an operator installs beside the cloud before
the adapter can serve anything, such as NVIDIA Dynamo for `kubernetes-dynamo`. The catalog declares
the name only, because it cannot know what a cluster runs. Composition records the runtimes the
control plane is configured to run through it (`configured_runtimes`); for Dynamo these are the
runtimes with a complete image and version pair, the same ones it registers Dynamo backends for.
It also records, for each runtime, the settings that configure it (`settings`), even when no
runtime is configured. A profile with settings is the control plane's own configuration, so a
runtime it does not list is known to be missing. The catalog has no settings and never knows. This
is operator configuration, not qualification evidence.

The planner offers a setup on such an adapter only when the request names the adapter, or the
setup's runtime is configured. A request that names only the cloud does not say the software runs
there, so the planner leaves those setups out and records a proposal warning that names them, and
the settings that would add them when the configuration is known. A request that names the adapter
keeps its setups; where a setup's runtime is known to be missing, a warning says it will fail to
deploy and names the settings.

The control API refuses a deployment, and a run whose setups it deploys, on an adapter whose
prerequisite is known to be missing for the setup's runtime (`prerequisite_not_configured`). The
check reads the provider profile, not the adapter's name. A run on an agent's endpoint is not
checked: the agent starts its setups in its own cluster.

A setup on an adapter with a prerequisite is not a plain server (`ProviderProfile.PlainServer`),
wherever the software runs. An InferCrane agent starts plain servers only. A run on an agent's
endpoint refuses such a setup, or one with a serving graph, before any GPU starts. A plan for an
agent's endpoint with a declared setup leaves them out. The profile decides, never the adapter's
name.

## Prohibited coupling

Lifecycle core must not switch on provider name. Secrets must not enter persisted provider metadata.
Provider pricing, capacity and timing are unknown unless returned by a trustworthy source with
timestamp and provenance.
