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

<Card title="Model artifacts" icon="cube" href="/features/model-artifacts">
  See the persisted identity and evidence model.
</Card>
