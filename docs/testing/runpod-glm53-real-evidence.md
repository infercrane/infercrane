# RunPod GLM-5.3 Flash real-infrastructure evidence

Date: 2026-08-27  
Verdict: **not qualified**  
Run: `glm53-runpod-20260827-r7`

This record preserves a bounded paid negative result. It must not be cited as proof that InferCrane
serves GLM-5.3 Flash. The raw, credential-free capture remains outside the repository at
`/tmp/infercrane-e2e-report/glm-acceptance/glm53-runpod-20260827-r7/` on the qualification host.

## Exact attempted tuple

| Component | Identity |
| --- | --- |
| Model | `zai-org/GLM-5.3-Flash@3f1971b7b5f7a528c9c4ef6212c8785298a8c24a` |
| Runtime package | `vLLM 0.1.dev20051+g487ecf187` |
| Runtime image | `vllm/vllm-openai@sha256:2c6da6c6f16ed15c91e412d896dba13701f25fe1861eaec9ddaa4db34d1d21c4` |
| InferCrane under test | `f54dda80a4e16841ecbc6a4b691606481c48f4d0` |
| Provider topology | RunPod Secure Cloud, one native Pod, 4 × NVIDIA H200, 500 GiB container disk |

## Measured boundary

- Durable deploy submission started at `2026-08-27T00:41:41Z`.
- RunPod created Pod `drgz7edz8pezmb` at `2026-08-27T00:42:14.901Z`.
- Provider system logs showed the OCI transfer beginning at `00:42:33Z`, extraction completing by
  approximately `00:45:28Z`, and the container model snapshot fetch beginning at
  `00:45:32.188Z`. The image path therefore consumed about 3m14s from Pod creation.
- The pinned Hugging Face snapshot requested 72 files without an authenticated Hub token. It did
  not complete before the paid watchdog boundary. GPU and GPU-memory utilization were still zero
  at `01:01:29Z`, consistent with model transfer rather than engine initialization.
- The suite stopped at `01:49:55Z` with exit code 124. The approximate paid exposure for this Pod
  was 68 minutes at the provider-observed $18.36/hour; provider billing remains authoritative.

Because the runtime never became ready, deploy-to-ready, TTFT, TPOT, request p95 at concurrency
1/8/32, streaming versus buffered behavior, model identity from `/v1/models`, and Release Guard
behavior were **not measured**. They remain required before any support or performance claim.

## Durable-operation and cleanup evidence

- The foreground CLI was deliberately terminated after submission. Its captured exit was non-zero,
  while the persisted operation continued and retained the deterministic provider resource name.
- The control plane was restarted during the unresolved deploy and resumed the durable operation.
- Budget-stop cancellation was accepted for operation
  `1c4820f92983ffdf7e0a79e167e79bff`.
- At `2026-08-27T01:50:09Z`, direct provider inventory returned zero Pods and zero Serverless
  endpoints. InferCrane deployment and orphan inventories were also empty.

## Engineering conclusions

1. The native Pod lifecycle, deterministic adoption, CLI-disconnect durability, process-restart
   durability, cancellation, and zero-resource teardown crossed a real provider boundary.
2. An uncached 300+ GiB model transfer is not an acceptable default launch path. It is also not
   evidence that the runtime image or model is incompatible.
3. The operation emitted repeated semantically identical checkpoints while waiting. The follow-up
   implementation deduplicates those events and exposes the persisted current step to the CLI.
4. The general remediation is exact-model persistent cache/prewarm support, not a GLM-only recipe:
   native RunPod Pods can bind a verified `model@commit` network volume, standard Hub downloads use
   the persistent path, and filesystem-materialized profiles honor `INFERCRANE_MODEL_DIR`.
5. A fresh paid qualification must start from that prepared volume, use an authenticated Hub path
   when transfer is required, and repeat the complete request/Release Guard/cleanup matrix.

The remediation described above was implemented after the paid run and therefore was not exercised
by this evidence. Its contract tests belong to the later repository commit; real cache timing and a
ready model tuple remain unqualified until a separate bounded run proves them.
