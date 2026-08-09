# Stage 1 POC

Stage 1 uses existing vLLM workers. It proves the stable logical deployment abstraction,
upstream routing, health reconciliation, restart persistence, OpenAI compatibility,
streaming, and normalized metrics. It does not provision GPUs or autoscale.

```bash
infercrane target add gpu-a --url http://127.0.0.1:8101 --runtime vllm
infercrane target add gpu-b --url http://127.0.0.1:8102 --runtime vllm
infercrane deploy Qwen/Qwen3-8B --name qwen-prod --targets gpu-a,gpu-b
infercrane route qwen-prod --strategy cache-aware
infercrane serve
```

The gateway is then available at `http://127.0.0.1:8080/v1` with the logical model
`qwen-prod`. Set `INFERCRANE_API_KEY` before serving; the development default is
`infercrane` and must not be used for an exposed installation.

The supported strategies are delegated to vLLM Router 0.1.15:

- `round-robin`
- `consistent-hash`
- `power-of-two`
- `cache-aware`

