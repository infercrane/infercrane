# v0.5.0-rc.1

v0.5 adds a focused browser workspace for operational evidence while preserving code-first and
control-plane ownership boundaries.

## Added

- Embedded responsive fleet and deployment dashboard at `/dashboard/`
- Lifecycle, operation, revision, Release Guard, performance, cold-start, benchmark, provider,
  scaling, event, orphan, and audit evidence views
- Explicit loading, empty, stale, disconnected, forbidden, and partial-data states
- Guarded cooperative operation cancellation with exact-ID confirmation
- System, light, and dark appearance with narrow-screen and reduced-motion behavior

## Security boundaries

- The dashboard calls only authenticated public API routes and never queries PostgreSQL.
- Credentials remain in tab-scoped session storage and are never placed in URLs or persistent
  browser storage.
- Static assets are self-contained and served with restrictive browser security headers.
- Complex mutations and deployment authoring remain in CLI, API, SDK, Terraform, and GitHub flows.

Paid-provider and broad browser-matrix qualification remain deferred to the consolidated v1.0
manual acceptance run.
