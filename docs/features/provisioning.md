---
title: Provisioning and readiness
description: Distinguish provider capacity, container startup, artifact preparation, runtime startup, and readiness without guessing.
---

# Know what a deployment is waiting for

Provisioning is a durable operation. The CLI can disconnect while the control plane continues to
reconcile one deterministic provider identity per replica intent. Reconnect with the operation ID
printed by `deploy`:

```bash
infercrane operation watch OPERATION_ID
infercrane status DEPLOYMENT --watch
infercrane logs DEPLOYMENT --follow
```

## Choose and check an adapter before provisioning

```bash
infercrane integrations --output json
infercrane plan MODEL --cloud PROVIDER --gpu ACCELERATOR --output json
```

`integrations` distinguishes registration, local evidence, and real-system qualification. `plan`
validates the requested intent without allocating capacity. Follow the exact adapter guide for
credentials, networking, immutable images, runtime contract, preflight, cleanup, and current limits:

- [RunPod elastic and Serverless](/integrations/runpod)
- [AWS EC2 BYOC](/integrations/aws-ec2)
- [GCP Compute BYOC](/integrations/gcp-compute)
- [Kubernetes and KServe](/integrations/kubernetes)
- [Exact-combination compatibility check](/compatibility#check-one-exact-serving-combination)

InferCrane cannot safely choose a provider from an unspecified workload. Decide these inputs first:
immutable model/artifact, runtime and version, compute mode, accelerator/topology, region/network,
replica bounds, protocol needs, and required evidence. Then use this boundary:

| Situation | Safest path |
| --- | --- |
| A compatible workload already exists | Adopt it observe-only; create no infrastructure |
| RunPod is acceptable for a preview | Use the explicit RunPod guide; elastic and Serverless remain experimental pending exact real evidence |
| Customer AWS or GCP is required | Use the narrow BYOC adapter guide; real identity, network, quota, GPU, and deletion qualification remain external |
| A Kubernetes GPU cluster already exists | Use the namespace-scoped Deployment/KServe guide; Kind proves API behavior, not real GPU execution |
| No exact combination has qualification evidence | Stop after `plan` or adopt an existing endpoint; do not infer support from registration |

You do not need to read every provider guide. Select one only after the inputs above identify the
operator-owned boundary:

| Adapter | Operator must already control | Potentially billable objects | Current evidence boundary |
| --- | --- | --- | --- |
| RunPod | Project, scoped key, and optional immutable Serverless template | Pod or Serverless endpoint/workers | Experimental; exact real lifecycle evidence required |
| AWS EC2 BYOC | Account, assumable role, private subnet/security group, AMI, instance profile, worker secret | EC2 instance and attached network/storage resources | Hermetic contract only; real AWS GPU/network evidence required |
| GCP Compute BYOC | Project, attached identity, private network, immutable VM/container inputs | Compute instance and attached resources | Hermetic contract only; real GCP GPU/network evidence required |
| Kubernetes/KServe | Existing cluster, kubeconfig context, namespace/RBAC, worker Secret, GPU nodes | Cluster workloads and the cluster's underlying capacity | Kind API evidence only; real GPU cluster evidence required |

If these requirements do not select exactly one adapter, stop and collect workload, security,
residency, capacity, and cost constraints; InferCrane will not silently choose infrastructure.

<Warning>
`deploy` and `apply` can create billable provider resources. Do not execute either until provider
credentials and read-only doctor checks pass, the plan is reviewed, current provider inventory is
recorded, and the exact combination's qualification gap is accepted. Cost remains unknown when no
trustworthy provider evidence exists.
</Warning>

`status` shows whether the deployment is serving and whether desired capacity is still converging.
`operation watch` follows the blocking mutation. `logs` follows durable events and is the best view
when you need the transition history rather than the latest snapshot.

## Read the current stage

| Reported stage | Grounded meaning | What to do |
| --- | --- | --- |
| Waiting for capacity | A provider stock observation is unavailable, constrained, or allocation is still pending | Keep watching; do not submit another deploy for the same intent |
| Provider accepted | The provider has accepted the deterministic resource identity | Inspect provider state only if the operation stops making progress |
| Preparing artifact | The worker is reachable but the expected model is not ready; download, cache materialization, or model load may still be in progress | Check runtime/container logs and disk/cache evidence |
| Starting runtime | The container is reachable but the OpenAI-compatible runtime has not passed health and model identity checks | Check runtime logs, memory, model arguments, and served model identity |
| Ready | Health succeeds and `/v1/models` reports the expected model | The worker may enter the published route generation |
| Unknown boundary | The provider or runtime does not expose a narrower stage | Treat the combined boundary as unavailable; do not infer a made-up duration |

Not every provider exposes container download, artifact transfer, model load, and runtime
initialization separately. InferCrane reports the narrowest boundary supported by fresh evidence.
For example, `provider_capacity_or_worker_initialization` means exactly that the product cannot
separate those phases from the available observation.

## Diagnose a long wait

1. Copy the blocking operation ID from `status`.
2. Run `operation watch` and note the last changed stage, provider message, attempt, and retry time.
3. Run `logs --follow` in a second terminal to distinguish a repeated observation from a real state
   transition.
4. Run `infercrane inspect DEPLOYMENT --output json` for the persisted provider identity and
   non-secret infrastructure details.
5. If the provider retains a failed resource—for example after an interrupted image pull—cancel the
   durable operation before replacing it. Do not start a second deploy with a different identity.

Known bootstrap failures such as interrupted image pulls or exhausted host storage remain visible
and retryable while the provider retains the resource. InferCrane does not silently select a
different accelerator or create a second billable resource.

## Capacity evidence is advisory

Before the first create, InferCrane discovers the deterministic resource. An existing resource is
adopted before mutable stock is consulted. If no resource exists, a provider adapter may report
`available`, `constrained`, `unavailable`, or `unknown`:

- `unavailable` defers creation with a retryable error;
- `constrained` proceeds with an explicit warning because stock is not a reservation;
- `unknown` permits the normal provider attempt but cannot support a capacity claim.

The first stock advisor queries RunPod secure-cloud availability. A region-qualified request is
still labeled global when the provider response cannot prove stock in the requested region.
Credentials are sent in headers and are not persisted in progress or events.

## Idempotency and cleanup

The provider resource key is stored before any external create call. If the create succeeds but its
response is lost, retry performs discovery and adopts the resource. Delete re-observes asynchronous
provider removal and cannot mark the replica deleted while the resource remains visible.

### Reconcile an interrupted create

1. Do not create a new deployment, revision, or idempotency key.
2. Reattach to the persisted operation and export the stored identities:

   ```bash
   infercrane operation watch OPERATION_ID --wait-timeout 15m
   infercrane inspect DEPLOYMENT --output json
   infercrane orphans --output json
   ```

3. In the provider's read-only inventory, locate the exact `provider_resource_id`, deterministic
   external key, ownership tags/labels, revision, and replica ordinal from `inspect`. A same-name
   resource with mismatched ownership is not adoptable.
4. If the identities match, let the existing operation resume. Reissuing the original deploy/apply
   request is safe only with its original idempotency key and identical intent. Replaying
   `rollout provision DEPLOYMENT REVISION_ID --wait` is safe for the same persisted revision; do not
   run `rollout create` again.
5. If provider inventory is unavailable, incomplete, or mismatched, stop mutation and preserve
   PostgreSQL. Resolve ownership manually before create or delete.

Provider-specific inventory is intentionally outside one generic command: use the RunPod Pod/
endpoint inventory, AWS EC2 tag-scoped inventory, GCP label-scoped instance inventory, or Kubernetes
namespace/UID/managed-fields inventory described by that adapter. An empty InferCrane `orphans`
response cannot prove a provider credential can see every external resource.

After a paid qualification run, use both views:

```bash
infercrane orphans --output json
infercrane deployments --output json
```

Then verify the provider account inventory directly. InferCrane inventory cannot prove the absence
of resources hidden by stale, incomplete, or mis-scoped provider credentials.

## Qualification boundary

Existing targets are implemented. Provider adapters have independent evidence states. SkyPilot
elastic lifecycle logic passes local lost-response, replay, and cleanup fixtures, but real RunPod
provider/region/GPU qualification remains external. Runtime inspection requires both a healthy
endpoint and the expected served model. Development fake workers prove control flow only; they do
not prove GPU performance, model compatibility, or provider reliability.

See [capability status](/project-status), [cold-start intelligence](/features/cold-starts), and the
[provider contract](/architecture/provider-contract).
