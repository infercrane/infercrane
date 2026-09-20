# Dashboard MVP integration

This is the implementation contract for the private InferCrane console. It
keeps the main journey small while preserving the control plane's evidence and
ownership boundaries.

## Product surface

The primary rail has five destinations:

| Destination | Customer job | Route |
| --- | --- | --- |
| Models | Use a hosted API or choose an open-weight model | `/model-apis` |
| Deployments | Create, observe, and improve serving workloads | `/workloads` |
| Sandboxes | Run private CPU tasks through a dedicated Brezel project | `/sandboxes` |
| Usage | Inspect traffic, performance, and spend | `/usage` |
| API keys | Create and revoke scoped credentials | `/settings/api-keys` |

There is no primary **Overview** or **Optimize** destination. `/overview`
redirects to `/workloads` for old links. Optimization is an action on a
deployment because the evidence, target hardware, and release decision belong
to that deployment.

The **New** dialog contains only four starts:

1. Use a Model API.
2. Deploy an open-weight model.
3. Connect existing inference.
4. Create a sandbox.

The command menu uses the same vocabulary. Advanced routes remain searchable
only when they correspond to an implemented workflow; they are not duplicated
as competing product categories.

## Deployment journey

```text
Choose model
    │
    ▼
Describe workload ── public prior by default
    │                 observed traffic when available
    ▼
Choose outcome ───── latency · throughput · cost · balanced
    │
    ▼
Choose location ──── InferCrane-managed · customer cloud · existing endpoint
    │
    ▼
Review plan ──────── hardware + runtime + recipe + estimated limit
    │
    ▼
Deploy ───────────── stable endpoint + immutable revision
    │
    ▼
Observe ──────────── real traffic shape and content-free metrics
    │
    ▼
Improve ──────────── candidates → Brezel build → exact GPU replay → Release Guard
```

The wizard asks only questions that change the outcome. Runtime, quantization,
parallelism, cache policy, kernels, and exact GPU topology are recommended
outputs. Advanced constraints are available from the review step.

### Workload evidence

Day zero starts from one immutable public workload prior:

- `public-interactive`
- `public-long-prefill`
- `public-decode-heavy`

The deployment detail then offers **Improve**. Before any candidate can be
called qualified, the public prior must be replaced or corroborated by a
customer replay or an exact customer benchmark. A public-trace match is useful
for candidate ordering, never proof of customer performance.

## Deployment detail

The first screen answers four questions:

1. Is it serving?
2. Is it meeting the declared target?
3. What does it cost?
4. Is there a qualified improvement?

The primary action is contextual:

- no traffic evidence: **Connect workload data**;
- enough evidence but no campaign: **Find improvements**;
- qualified candidate: **Review candidate**;
- failed release guard: **Inspect evidence**.

Hardware, runtime, recipe, kernel source, benchmark identity, and rollback state
are evidence rows below the decision, not a wall of top-level cards.

## Brezel product boundary

InferCrane is the customer product and policy plane. Brezel is a separately
deployed execution provider.

```text
Customer
   │ authenticated InferCrane request
   ▼
InferCrane ─ tenancy · approved templates · audit · stable API
   │ provider credential from owner-only file
   ▼
Brezel ───── private project · microVM lifecycle · files · commands · previews
   │
   ├── Customer sandbox (explicit lifecycle in the Sandboxes page)
   │
   └── Internal optimization worker (fixed runner, no customer argv)
             │
             └── content-addressed artifact → separate exact-GPU benchmark
```

The two uses never share customer-visible resource identity. Internal
optimization jobs accept typed manifests and a baked-in runner only. Customer
sandboxes are explicit lifecycle resources.

### Initial native sandbox contract

The implemented InferCrane API supports:

- provider capability discovery;
- create from an approved immutable environment revision;
- InferCrane-owned names, purpose, source, workspace, and tenant authorization;
- list and detail;
- incremental command tasks, bounded file transfer, private previews, activity,
  and redacted receipts;
- durable workspace creation, compensation, pause, resume, and delete;
- an append-only content-free usage ledger;
- no-internet networking only;
- provider states and failures without optimistic rewriting;
- required idempotency keys and InferCrane audit events.

Routes:

```text
GET    /api/v1/sandboxes/capabilities
GET    /api/v1/sandboxes
POST   /api/v1/sandboxes
GET    /api/v1/sandboxes/{id}
POST   /api/v1/sandboxes/{id}/commands
PUT    /api/v1/sandboxes/{id}/files?path=...
GET    /api/v1/sandboxes/{id}/files?path=...
POST   /api/v1/sandboxes/{id}/ports/{port}/leases
GET    /api/v1/sandboxes/{id}/events
GET    /api/v1/sandboxes/{id}/receipt
GET    /api/v1/sandboxes/usage
POST   /api/v1/sandboxes/{id}/pause
POST   /api/v1/sandboxes/{id}/resume
DELETE /api/v1/sandboxes/{id}
```

Existing `/api/v1/sandboxes/references` routes remain for externally owned E2B,
Modal, Kubernetes, or other sandboxes. Native and external resources are shown
as separate ownership modes.

### Honest release boundary

The native provider is a **private-tenant preview**. One configured InferCrane
tenant maps to one dedicated Brezel project. The MVP does not claim shared
hosted multitenancy, GPU passthrough, interactive PTY/SSH, arbitrary OCI builds,
controlled arbitrary egress, host-loss durability, or hardware attestation. The
UI uses “Console” for bounded tasks and explicitly says it is not an interactive
terminal.

Configuration is all-or-nothing:

```text
INFERCRANE_BREZEL_SANDBOX_URL
INFERCRANE_BREZEL_SANDBOX_TOKEN_FILE
INFERCRANE_BREZEL_SANDBOX_PROJECT_ID
INFERCRANE_BREZEL_SANDBOX_TENANT_ID
INFERCRANE_BREZEL_SANDBOX_TEMPLATES_JSON
INFERCRANE_BREZEL_SANDBOX_DEFAULT_TEMPLATE
INFERCRANE_BREZEL_SANDBOX_MODEL_CONNECTORS_JSON
```

The token file must be an owner-only regular file. Production uses HTTPS;
loopback HTTP is accepted for local development. Template values are immutable
`envr_…` revisions rather than mutable image tags.

## Console implementation order

1. Simplify navigation and redirect `/overview` to `/workloads`.
2. Merge Model APIs and open-weight selection into one Models page with two
   tabs; keep the current routes as internal/deep-link compatibility.
3. Add private-computer list, guided create, and Console, Files, Preview,
   Activity, and Access detail tabs against the native product API. Show the
   not-configured and tenant-not-enabled states explicitly.
4. Add public workload-prior selection to Build and place **Improve** on the
   deployment detail.
5. Reduce the New dialog and command menu to the same five-product vocabulary.
6. Keep every native Brezel path server-side, audited, tenant-authorized, and
   qualified before exposing it through the console.

## MVP acceptance

- A new user reaches Models, not Overview.
- A returning user reaches Deployments.
- No top-level label duplicates Models or Optimize.
- Every primary navigation item has a real loading, empty, unavailable, and
  populated state.
- A sandbox cannot be created from an unapproved environment revision or for a
  tenant other than the configured private tenant.
- A public workload prior can rank experiments but cannot produce a qualified
  performance claim.
- The console never labels a non-interactive Brezel command stream as a
  terminal.
- All mutations require an idempotency key and expose provider cleanup state.
