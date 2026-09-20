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

Register a reference to a fine-grained read token already injected into the control-plane
environment. The API and database receive the reference, never the token value:

```bash
export HF_TOKEN='injected-by-your-secret-manager'
curl -fsS "$INFERCRANE_API_URL/api/v1/secrets" \
  -H "Authorization: Bearer $INFERCRANE_API_KEY" \
  -H 'Content-Type: application/json' \
  -d '{"name":"model-huggingface-production","resolver":"env","reference":"HF_TOKEN"}'
```

Pass the returned ID as `model_secret_reference_id` when creating the deployment. InferCrane
resolves that credential only at the artifact boundary, verifies repository access, and persists the
immutable revision plus the reference ID. It does not persist or return the token.

Artifact access and runtime delivery are separate controls. RunPod should receive a provider-native
secret name, Kubernetes should use a namespace-scoped Secret, and offline AWS/GCP workers should use
an immutable pre-materialized model cache. A successful Hub resolution does not claim that an
unconfigured runtime can download the repository.

<Card title="Model artifacts" icon="cube" href="/features/model-artifacts">
  See the persisted identity and evidence model.
</Card>
