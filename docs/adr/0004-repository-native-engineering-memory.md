# ADR 0004: Repository-native engineering memory

Status: Accepted  
Date: 2026-08-09

## Context

The repository will be developed by a growing team using humans and multiple coding agents.
Chat history and vendor-specific memory are incomplete, non-reviewable, and unavailable to other
tools. Loading an entire large repository into every model context is costly and unreliable.

## Decision

Store durable knowledge in version control using a layered system: a small `AGENTS.md` contract,
a shared documentation index, immutable ADRs, feature documents, explicit status/ownership, and a
deterministically generated repository map. Claude Code imports the shared contract. CI rejects
stale generated knowledge and runs the same verification workflow contributors use locally.

## Consequences

Behavioral changes include documentation work. Generated summaries remain navigation aids rather
than authority. All tools receive the same memory, and architectural history survives team and
vendor changes.

## Rejected alternatives

- One giant prompt file: exceeds useful context and becomes stale.
- Chat transcripts as memory: not authoritative or consistently accessible.
- Vector storage as the primary source: difficult to review and can retrieve obsolete decisions.
