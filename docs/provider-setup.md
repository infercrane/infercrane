---
title: Provider setup
description: Configure credentials and prerequisites for qualified infrastructure adapters.
---

# Provider setup

Provider integrations are registered control-plane adapters. Each provider owns its credentials, infrastructure semantics, and billable resources; InferCrane owns durable intent, reconciliation, and cleanup. A provider is supported only after its adapter combination appears in the release qualification matrix.

## RunPod

Set a scoped `RUNPOD_API_KEY` on the control plane and configure SkyPilot's RunPod credentials for elastic workers. Run `infercrane doctor --cloud` before provisioning.

For Serverless, create a RunPod vLLM template with `MODEL_NAME`, immutable `MODEL_REVISION`, and `RAW_OPENAI_OUTPUT=1`, then set `INFERCRANE_RUNPOD_SERVERLESS_TEMPLATE_ID`. `infercrane doctor --serverless` reads and validates the template without creating an endpoint.

Set `INFERCRANE_URL` to a URL reachable from AIPerf and clients. Keep provider and InferCrane credentials out of specifications, logs, issue reports, and benchmark artifacts. Always inspect existing pods/endpoints before retrying manual acceptance and delete paid resources after the test.

## AWS EC2 BYOC

AWS elastic support uses a separately registered EC2 adapter rather than provider conditionals in the
lifecycle engine. It requires a complete role, private network, AMI, instance profile, worker secret,
instance type/GPU, region, and immutable image configuration. See [AWS EC2 BYOC](/integrations/aws-ec2)
and run `infercrane doctor --aws` before provisioning.

ASG, EKS, SageMaker, and Bedrock have separate registered profiles. Registration documents their
ownership boundary; it is not executable qualification. Inspect `infercrane integrations`.

## GCP Compute BYOC

The `gcp-compute` adapter launches private, digest-pinned workers with an attached service account
and deterministic adoption identity. Configuration is all-or-nothing. See
[GCP Compute BYOC](/integrations/gcp-compute). MIG, GKE, and Vertex remain separate registered,
deferred profiles rather than implicit aliases.

## CoreWeave

The `coreweave-cks` profile is CKS-first: InferCrane reuses its namespaced Kubernetes lifecycle and
does not install or own the provider-managed GPU operator. The profile is registered but not yet
executable or locally qualified; real CKS qualification remains deferred.

## Kubernetes

The Kubernetes adapter uses an explicit kubeconfig context, one namespace, an immutable default
runtime image, and a worker Secret reference. It owns a bounded Deployment/Service set or one optional
KServe InferenceService. Apply the reviewed namespace and RBAC manifests, then run
`infercrane doctor --kubernetes`. See [Kubernetes](/integrations/kubernetes) for exact configuration,
security boundaries, local Kind qualification, and current real-GPU limitations.
