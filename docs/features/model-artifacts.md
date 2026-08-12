---
title: Model artifacts
description: Resolve mutable model references to immutable, evidence-bearing artifact identity.
---

# Model artifacts

InferCrane resolves a Hugging Face repository and requested revision through
`huggingface_hub.HfApi.model_info`. The resulting commit SHA is attached once to the immutable
deployment revision and passed to vLLM with `--revision`. Retries reuse the persisted artifact;
they do not resolve a mutable branch again.

Persisted evidence includes the repository, requested reference, immutable commit, canonical model
identity, approximate Hub storage when returned, and grounded library/pipeline metadata. Cache state
is `unknown` unless the execution backend can measure the worker cache. The control-plane host cache
is never presented as worker cache evidence.

The production image installs `huggingface_hub` and `hf_xet` in an isolated virtual environment so
their dependency versions cannot alter SkyPilot's environment. Private or gated repositories use
the standard Hugging Face token environment/configuration; InferCrane does not implement a model
download protocol.
