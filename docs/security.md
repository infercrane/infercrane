---
title: Security
description: Authentication, tenant isolation, secret references, network boundaries, and secure deployment guidance.
---

# Security

InferCrane authenticates the control and data planes with bearer credentials and tenant-scopes public API reads and writes. Use separate, least-privilege provider credentials and rotate them through your secret manager. Production PostgreSQL must use TLS; back up and restore it as the lifecycle source of truth.

For the path from a local proof to a shared endpoint, pair this page with
[Share a development endpoint safely](/runbooks/shared-development),
[endpoint admission and distributed tenant quotas](/features/admission-async),
[protocol qualification](/features/protocols#qualify-streaming-cancellation-quotas-and-retries-together),
and the [Request Inspector contract](/features/adoption-diagnostics#inspect-one-request).

The identity boundary is a tenant. Principals are service accounts with a role ceiling and
explicit action scopes. A scope can remove a role permission but cannot add one. Credentials are
shown once, stored only as SHA-256 digests, and support rotation and revocation.
Existing legacy principals are migrated to their previous explicit action set; new secret and
external-capacity permissions are never granted implicitly during upgrade.

`infercrane login` issues a CLI key only after a person approves the sign-in in the dashboard
([Sign in the CLI](/features/cli-login)). The two sign-in endpoints that the CLI calls before it
has a key are unauthenticated and bounded: at most 500 sign-ins wait for approval at once, each
expires after 10 minutes, polling faster than every 2 seconds is told to slow down, and finished
requests are pruned after 24 hours. The CLI's device code is a 256-bit secret stored only as a
SHA-256 digest. Approval records the approver's tenant, role, and scopes; the key is minted on the
CLI's next poll in the same transaction that consumes the request, so one approval yields at most one
key. The key is an ordinary principal of kind `cli_session` with the approver's role and scopes,
never more, and expires after 90 days. Only dashboard principals (people, service accounts, and the
bootstrap key) can approve; CLI keys and endpoint tokens cannot. Keys are revoked by
`infercrane logout` or in **Settings → CLI sessions**. Last use is recorded on the control API at most
once a minute per key and never on the inference path. Removing an organization member does not
revoke their CLI keys; revoke them in **Settings → CLI sessions**. The CLI keeps the key in the OS
keychain, written on standard input rather than the command line, and falls back to its `0600`
config file.

The [InferCrane agent](/architecture/agent) observes model servers on a customer's own machines
without any inbound access: it only makes outbound HTTPS calls to `/api/v1/agent/sync` and
`/api/v1/agent/servers`, and sends its key only over HTTPS (plain HTTP only to a control plane on
the same machine). An agent key is a principal of kind `agent`, minted with the `deploy` action and
shown once. It is refused on every other control-plane route and on the inference gateway, so a
leaked agent key can register at most 32 observe-only endpoints and report about them, nothing
else. The control plane never dials, probes, or routes to a server it learned through an agent. The
agent filters engine metrics on the machine to the families InferCrane parses and never sends
prompts, completions, headers, or a server's API key, which it sends only to the servers named
with `--server`. Request hashes from its optional local gateway are keyed with a random secret
created on the machine and never sent, independent of the agent key. Revoking an agent disables
its principal and is checked on every agent request, so the agent stops at its next sync. Agent keys do not expire; rotate them by creating a new agent and revoking the
old one.

An agent that serves an [agent-managed endpoint](/architecture/agent#the-agent-as-the-data-plane)
receives the SHA-256 digests of the workspace's live, non-agent API keys that may call that
endpoint, the same digests the control plane stores, so it can authenticate requests inside the
customer's cloud. No key itself leaves the control plane, keys restricted to other endpoints are
not sent, and an agent with no agent-managed endpoint receives none. A leaked agent key therefore
also exposes those digests, which cannot be turned back into keys or used against the control
plane, which never accepts a digest. The agent stops accepting keys when it cannot refresh the
digests for 15 minutes, so a revoked key stops working on the agent within that window, and at
once when the agent itself is revoked. Request quota is leased from the shared allocator; an agent
without a lease returns `429` rather than serving unmetered. An endpoint with residency
`customer-cloud` accepts only agent-owned bindings (enforced by a database trigger), and the hosted
gateway refuses it instead of forwarding its prompts to any supply.

Candidate runs the control plane queues for an agent are bounded specs, never shell commands: the
image must be pinned by digest and on the agent operator's allowlist, the entry point cannot be a
shell or interpreter, and arguments cannot carry substitutions. The agent runs them with a
namespaced Role (Deployments, Services, read-only PVCs) and returns only numbers; eval prompts,
answers, and logs stay in the cluster.

`infercrane connect --token-env VAR` sends a server's API key to the control plane once. It is
stored only as AES-256-GCM ciphertext bound to the workspace and endpoint, sealed with
`INFERCRANE_CREDENTIAL_ENCRYPTION_KEY`, never returned, and used only toward that server: for
discovery and health probes, as the bearer token on requests the gateway routes to it when its
binding is traffic-managed, and by a migration Proof's graded quality check of that server. It is
held in the gateway's in-memory route snapshot and is never logged or shown. A key that cannot be
opened makes the server unroutable and unchecked, never reached without it. Without the encryption
key the control plane refuses the credential, and the agent, which keeps the key on the server's
machine, is the path.

Compute configured on the control-plane process (cloud workload identity, a kubeconfig context,
SkyPilot manifests) is not tenant-scoped. Only the bootstrap operator may deploy or optimize on it
without a tenant compute connection, unless `INFERCRANE_OPERATOR_COMPUTE_TENANTS` names the
workspace or is `*`. The wildcard is refused when the control plane sells managed capacity or
auto-provisions workspaces at sign-up. The grant never extends to InferCrane-managed prepaid
capacity or the RunPod supply key. See
[Let workspaces use operator compute](/provider-setup#let-workspaces-use-operator-compute).

A worker is started with one bearer credential. A worker's address belongs to its workload, which
can be a workspace's own image, so whatever the control plane sends there is readable by that
workload. Every call the control plane makes to a worker's address (routing, health checks, engine
scrapes, direct benchmarks, optimization measurement, answer checks, and playground prompts)
therefore presents the credential scoped to that worker's workspace and deployment. The operator's
shared credential is sent only to workers on operator compute that were started with it, never to a
worker that records a scoped credential and never to a worker in a tenant's account.

- **RunPod Pods**, on the operator's account and on a tenant's, are started with a credential
  derived from `INFERCRANE_WORKER_CREDENTIAL_KEY` for that workspace and deployment. A Pod never
  holds `INFERCRANE_API_KEY`, and one deployment's credential opens no other deployment's workers.
  `INFERCRANE_MANAGED_DEPLOYMENTS_ENABLED` is refused at startup without the derivation key.
  Without the key, only the bootstrap operator can start Pods on the operator's account, and those
  Pods hold the control-plane worker credential described next.
- **AWS, Google Cloud, Kubernetes, and SkyPilot** workers read one worker credential the operator
  provisions for the adapter, and that credential is `INFERCRANE_API_KEY`. Code that runs in such a
  worker can therefore act as the bootstrap operator. Run only workloads you trust as operators
  there, and treat an operator compute grant as granting that trust to the workspace.
- A worker in a tenant's own account that records no scoped credential is not called at all, and
  a scoped worker whose credential cannot be derived is treated as unhealthy. The control plane
  does not fall back to the shared credential.
- While a deployment has older workers on the shared credential and newer ones on a scoped
  credential, only the scoped workers are routed. Replace the older workers to restore capacity.

Gateway authentication uses an in-memory last-known-good credential snapshot so an established data
plane does not add PostgreSQL latency or fail immediately during a database outage. Rotation and
revocation take effect on the next successful snapshot refresh. If immediate revocation is required
while PostgreSQL is unreachable, restore authoritative database access or fence/restart the affected
gateway instances; InferCrane cannot infer a revocation it cannot read.

Inference errors name no internal host, URL, or service. When an upstream fails, the gateway answers
with its own OpenAI-shaped error (503 `upstream_unavailable`, or 504 when the endpoint deadline
passes). It never forwards an error body that a model server did not write, such as the router's
report of an unreachable worker, which names the in-cluster service URL. A failed async job's
`error_message` is only the gateway's HTTP status or a fixed message. The detail is logged under the
request or job ID for operators. See [gateway failure behavior](/features/gateway#failure-behavior).

A workspace secret is sealed in InferCrane, or a reference to where the secret already lives:

```bash
export HF_TOKEN='hf_...'
infercrane secret create hf-token --sealed-from-env HF_TOKEN       # sealed
infercrane secret create hf-token --kubernetes hf-token/HF_TOKEN   # stays in your cluster
infercrane secret create hf-token --aws-secret arn:aws:secretsmanager:...  # stays in your AWS account
export OPENROUTER_API_KEY='...'
infercrane secret create openrouter --from-env OPENROUTER_API_KEY  # operator only
```

- `sealed` stores the value only as AES-256-GCM ciphertext, sealed with
  `INFERCRANE_CREDENTIAL_ENCRYPTION_KEY` and bound to the workspace and secret name, so a row moved to
  another workspace or renamed does not open. The value is write-only: never returned, logged, or
  audited. Without that key the API answers `sealed_secrets_unavailable`, and a value sealed under
  another key reference does not open. To replace a value, delete the secret and create it again.
- `kubernetes` (`SECRET/KEY` in the cluster's InferCrane namespace) and `aws-secrets-manager` (an ARN)
  name a secret in the customer's own cluster or AWS account. Workers read it there; the control plane
  never resolves it and needs no permission on it. Only workspaces the operator lets deploy on its own
  cluster or AWS account may create one (`worker_secret_reference_not_allowed` otherwise).
- `env` stores the variable name, never its value.

Resolved values stay in process memory and are excluded from API responses, logs, audit payloads and
qualification evidence.

A model credential (`model_secret_reference_id`) reaches a worker only through its provider's own
secret store: a RunPod secret in the account that runs the Pod (the workspace's own account for a
compute connection), whose Pod specification holds only the `{{ RUNPOD_SECRET_... }}` placeholder; a
Kubernetes `secretKeyRef`; or a Secrets Manager ARN the EC2 instance profile reads and passes to the
runtime through the inherited environment, never a command line. The runtime gets it as `HF_TOKEN`,
which `huggingface_hub` sends only to `huggingface.co`; InferCrane never sets `HF_ENDPOINT`. Every other
adapter refuses a model credential before provisioning.

Model weights and LoRA adapters in a customer's S3 or GCS bucket are never read by the control plane.
An import stores a file manifest (paths, sizes, sha256) and the import's checks, never file contents.
Workers download the listed files with their own cloud identity (a Kubernetes service account, an
optional operator-named Secret for an S3-compatible store, or the EC2 instance profile) and check
every sha256 before the server starts; the serving container never receives object store keys.

An `env` reference reads the control-plane process environment, which also holds the operator's own
keys. So only the operator's workspaces may create or resolve one: the workspace of the operator API
key, the Model API operator workspace (`INFERCRANE_MODEL_API_OPERATOR_TENANT_ID`), and the workspaces
named in `INFERCRANE_ENV_SECRET_TENANTS`. Any other workspace gets `env_secret_reference_not_allowed`
when it creates one, and a reference it already holds stops resolving instead of returning a value.
Left unset, `INFERCRANE_ENV_SECRET_TENANTS` is `*` on a control plane that serves one customer and
empty where it sells managed capacity or provisions workspaces at sign-up; there, startup also refuses
`*`. Workspaces on such a control plane keep their provider keys in their own compute connection and
their other credentials as sealed secrets.

## Validate provider access without enabling traffic

Infrastructure adapters expose provider-specific read-only checks:

```bash
infercrane doctor --cloud       # SkyPilot/provider credentials
infercrane doctor --serverless  # RunPod template and API access
infercrane doctor --aws         # AWS identity and configuration reads
infercrane doctor --gcp         # GCP identity and Compute API reads
infercrane doctor --kubernetes  # Kubernetes API and RBAC reads
```

For an external model API, create only a reference-backed connection and inspect its redacted
metadata:

```bash
export OPENROUTER_API_KEY='injected-by-your-secret-manager'
infercrane provider connect openrouter-main \
  --adapter openrouter \
  --model openai/gpt-oss-120b \
  --from-env OPENROUTER_API_KEY
infercrane provider list --output json
```

Creating and listing the connection does not send a prompt, test request, or authorize spend. It
proves that InferCrane can resolve the named reference, not that the upstream model can serve a real
prompt. Connecting an existing endpoint can use that reference for one bounded authenticated
`/v1/models` discovery request. This verifies reachability and metadata access without enabling
traffic or spend; it is not a generation probe. Validate real serving through an explicitly approved,
budgeted staging request. Never put a credential in a URL, CLI argument value, DeploymentSpec,
ticket, log, or qualification artifact.

Prompt and output content are not recorded by default. Request telemetry stores identifiers, dimensions, status, timing, and token counts. AIPerf uses metrics-only record exports; InferCrane does not upload benchmark data. Shadow traffic is not implemented, so requests are never duplicated silently.

The built-in answer check of an optimization run sends a small fixed prompt suite to candidate and
reference revisions and compares the answers in memory; answers are never stored. A customer eval
set is the one place InferCrane keeps prompts you choose to give it: `infercrane evaluation set
upload` stores the uploaded file in plaintext, bounded to 512 KiB and 200 prompts, scoped to your
organization, and readable only by the worker. The API returns its metadata, never its prompts, and
`evaluation set delete` removes it. Do not upload prompts containing personal data or secrets. An
eval set's conversations, tool definitions, and earlier tool results are stored the same way. Tool
calls the models make during the check are compared in memory and never executed: the worker
validates their arguments against the JSON Schema the eval set declares, with no loader, so a
schema's remote `$ref` is refused at upload and validation fetches nothing.

Durable async inference is the explicit exception. It remains disabled until an operator injects
`INFERCRANE_ASYNC_ENCRYPTION_KEY`, and every submission must acknowledge encrypted content storage.
Request and result bodies are stored only as AES-256-GCM ciphertext bound to tenant and job identity.
Completion webhooks are signed, HTTPS-only, redirect-free and resolved through an SSRF-hardened
transport. Webhook payloads contain results, so their destinations are part of the application data
boundary. Synchronous inference remains content-free by default.

Tenant compute credentials are a second explicit encrypted boundary. Self-serve BYOC remains
disabled until the operator injects separate `INFERCRANE_CREDENTIAL_ENCRYPTION_KEY` and
`INFERCRANE_WORKER_CREDENTIAL_KEY` values. A submitted RunPod key
is verified before storage, encrypted with AES-256-GCM, and bound through associated data to the
tenant, provider, and connection name. API responses expose only connection metadata. Provider
calls decrypt the credential in memory, while worker authentication uses a separate deterministic
tenant-and-deployment-scoped credential. That credential, never the control-plane API key, is what
every direct call to the tenant's worker presents. Operator-owned provider keys, volumes, and
provider secret references are never copied into customer accounts.

Optimization run notification destinations use the same key. A destination URL can itself be a
bearer credential (a Slack incoming webhook is one), so the URL and the generated signing secret are
both stored as AES-256-GCM ciphertext bound to the tenant and destination ID. API responses show only
the host and the last characters of the path; the signing secret is returned once, at creation.
Deliveries go only to public HTTPS addresses through the same checked transport as signed alerts, and
carry run metadata and outcome summaries, never prompts, responses, or credentials. Changing a
destination requires an admin principal with the `manage_secrets` scope.

External fallback is disabled by default. Enabling it requires a persisted acknowledgement that
prompt and output data can leave controlled infrastructure, plus atomic hard request and worst-case
cost reservations. Selection happens before transmission; InferCrane does not replay a stream or
retry after bytes may have reached an external provider.

The complete commands are in [Keep sensitive inputs self-hosted](/integrations/overview#keep-sensitive-inputs-on-self-hosted-infrastructure)
and [Stable endpoints](/features/endpoints#add-an-authenticated-managed-api). A provider connection
alone enables neither traffic nor spend. If data residency cannot be proven, keep a one-binding
`manual` plan containing only self-hosted capacity. If trustworthy external pricing or a defensible
worst-case per-request reservation is unavailable, do not pass `--enable-external`; cost limits are
authorization ceilings, not provider invoice evidence.

AWS BYOC uses STS role assumption for each provider call and passes temporary credentials only to
the child AWS CLI process. EC2 receives no public IP. The worker retrieves its API key from AWS
Secrets Manager through a narrowly scoped instance profile; the control plane stores the secret ARN,
not the value. Require an external ID, restrict trust and permissions, and use tag conditions in
production.

The Kubernetes adapter always uses an explicit context and namespace. Its reference Role contains no
wildcards and cannot read Secrets, mutate RBAC, or create Pods directly. Workers read one named Secret
through their service account; the control plane stores only its name and key. Strict server-side
apply preserves field ownership and never force-steals conflicts. InferCrane deletes only exact names
with matching durable ownership labels and annotations.

The control plane contacts `huggingface.co` over HTTPS when an authenticated principal searches
Hugging Face models (`GET /api/v1/catalog/hugging-face/search`). It sends only the search text, the
pipeline filter, and a fixed `User-Agent`, never a Hugging Face token or tenant data. If your network
policy blocks that destination, the search returns a typed `hugging_face_search_unavailable` error
and nothing else changes. See [Search Hugging Face](/features/model-catalog#search-hugging-face).

When the operator sets `INFERCRANE_HUGGING_FACE_METADATA_ENABLED=true`, the control plane also reads
model info, `config.json`, and the safetensors summary from `huggingface.co` to check a model outside
the reviewed catalog (planning, `GET /api/v1/catalog/hugging-face/check`, and optimization
proposals). It is off by default. For a gated or private model, the workspace's saved Hugging Face
token is resolved at that read and sent only as an `Authorization` header to the same host:
redirects to another host are refused, and the token is never logged, cached, stored, or returned.
Reads made with a token are cached per workspace and credential. See
[Check any model automatically](/features/model-catalog#check-any-model-automatically).

Run the container as its non-root user, pin immutable image tags, restrict network access, and protect `/metrics` according to your environment. Report vulnerabilities through the repository's private GitHub security-advisory flow; do not open a public issue.
