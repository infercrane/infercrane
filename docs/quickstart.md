# Quickstart

InferCrane v0.1 supports vLLM on RunPod. It does not require Kubernetes.

```console
brew install infercrane
infercrane init --url https://infercrane.example
infercrane doctor --cloud
infercrane deploy Qwen/Qwen3-8B
infercrane status qwen3-8b --watch
```

The control plane must have PostgreSQL, RunPod credentials, SkyPilot, AIPerf, and a vLLM Router available. See [provider setup](provider-setup.md). The deploy command returns a durable operation; disconnecting the CLI does not cancel or corrupt it.

Once ready, send an OpenAI-compatible request to the logical endpoint:

```console
curl "$INFERCRANE_URL/v1/chat/completions" \
  -H "Authorization: Bearer $INFERCRANE_API_KEY" \
  -H 'Content-Type: application/json' \
  -d '{"model":"qwen3-8b","messages":[{"role":"user","content":"Hello"}],"stream":true}'
```

Inspect durable evidence with `infercrane events qwen3-8b`, `infercrane inspect qwen3-8b`, and `infercrane explain qwen3-8b`. Delete with `infercrane delete qwen3-8b --yes`; confirm provider inventory reaches zero before ending a paid test.
