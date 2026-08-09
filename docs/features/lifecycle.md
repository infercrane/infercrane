# Deployment lifecycle

<Snippet file="_snippets/safe-retry.mdx" />

Create and apply requests are validated and persisted before external provisioning begins. A leased worker resumes incomplete steps after a control-plane restart. Each replica intent has one deterministic provider identity, so replay adopts the same resource rather than submitting another.

Readiness requires the provisioned runtime to serve the expected model. Updates create immutable candidates, Release Guard records a deterministic decision, promotion switches routing generations atomically, and old capacity drains before termination. A bad candidate never replaces the active revision.

Delete first withdraws desired routing, then drains and removes external resources. Restarting midway resumes cleanup. Completion requires provider absence, removed targets and replicas, and a deployment tombstone. Operators should still verify provider inventory after paid acceptance tests.
