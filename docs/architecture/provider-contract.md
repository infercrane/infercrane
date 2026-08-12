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

## Prohibited coupling

Lifecycle core must not switch on provider name. Secrets must not enter persisted provider metadata.
Provider pricing, capacity and timing are unknown unless returned by a trustworthy source with
timestamp and provenance.
