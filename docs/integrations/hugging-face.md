---
title: Hugging Face models
description: Resolve mutable Hugging Face model references to immutable artifacts.
---

# Hugging Face models

InferCrane uses `huggingface_hub` and `hf_xet` rather than a custom transfer protocol. Before provisioning, it resolves a repository and mutable reference such as `main` to an immutable commit when the provider exposes that evidence.

```bash
infercrane plan Qwen/Qwen3-8B --cloud runpod --gpu L40S
infercrane deploy Qwen/Qwen3-8B --cloud runpod --gpu L40S
```

The resulting `ModelArtifact` can include repository identity, immutable revision, approximate size, and grounded compatibility/cache metadata. Missing provider evidence remains unavailable; InferCrane does not infer it.

## Private and gated repositories

Save a fine-grained read token as a sealed workspace secret. It is encrypted at rest, bound to the
workspace, and never returned:

```bash
export HF_TOKEN='hf_...'
infercrane secret create hf-token --sealed-from-env HF_TOKEN
```

Pass the returned ID as `model_secret_reference_id` when creating the deployment (`--model-secret`
in the CLI). The control plane uses the token to pin the commit and read the model's metadata. On
RunPod Pods it is stored as a RunPod secret in the account that runs the Pod, and the Pod names only
that secret. A Kubernetes cluster or AWS account keeps the token itself: name it with a `kubernetes`
reference (a `secretKeyRef`) or an `aws-secrets-manager` reference (an ARN the instance profile
reads), and the control plane never sees it. The token is never stored in plaintext or returned, and
the runtime sends it only to Hugging Face. See
[model artifacts](/features/model-artifacts#private-and-gated-models).

<Card title="Model artifacts" icon="cube" href="/features/model-artifacts">
  See the persisted identity and evidence model.
</Card>
