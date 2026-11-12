# DeepSeek-V4.1-Flash-1017 NVFP4

* vLLM (needs MXFP4 Engram support, [vllm-project/vllm#61530](https://github.com/vllm-project/vllm/pull/61530))
* SGLang loads it natively from 2026-11-04 on ([sgl-project/sglang#47935](https://github.com/sgl-project/sglang/pull/47935)).

The model is quantized with nvidia-modelopt **v0.48.0rc0**. Source revision: `1bae0e1088993b9b82de08f59de5be5a34e42170`.

| Benchmark | FP8 | NVFP4 |
|---|---|---|
| GSM8K | 96.1 | 95.8 |
