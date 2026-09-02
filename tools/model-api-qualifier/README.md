# DeepSeek Model API qualifier

This operator-only tool qualifies one exact, deliberately narrow contract:

- supplier: `deepseek`
- adapter: `deepseek-openai`
- alias: `deepseek-v4-flash`
- pinned revision: `DeepSeek-V4-Flash-0731`
- protocol: buffered and streaming OpenAI Chat Completions
- unsupported in this MVP: tools, response formats, vision, and thinking

It probes exact inventory, runs bounded buffered and streaming samples through
the production supplier adapter, verifies complete usage and terminal SSE
framing, and measures TTFT plus output-token throughput. The API key is read
from `DEEPSEEK_API_KEY`, copied only inside the adapter boundary, and never
written to evidence or included in an error.

No call is made unless `--confirm-live-deepseek` is present. Every live run is
billable. Use a dedicated, capped supplier key and an operator-controlled
machine.

```sh
DEEPSEEK_API_KEY='replace-in-shell-only' \
go run ./tools/model-api-qualifier \
  --confirm-live-deepseek \
  --offer-id deepseek-direct-v4-flash \
  --offer-version 1 \
  --qualification-id deepseek-direct-v4-flash-q-20260902 \
  --tuple-key 'deepseek|deepseek-v4-flash|openai|global' \
  --expected-revision DeepSeek-V4-Flash-0731 \
  --samples-per-mode 3 \
  --max-output-tokens 64 \
  --request-timeout 60s \
  --total-timeout 10m \
  --valid-for 1h \
  --evidence-ref 's3://infercrane-qualification/deepseek/2026-09-02.json' \
  --evidence-output ./artifacts/deepseek-raw.json \
  --qualification-output ./artifacts/deepseek-qualification.json
```

Upload the second file with the operator CLI:

```sh
infercrane model-api publish qualification \
  --file ./artifacts/deepseek-qualification.json
```

Both files are created with mode `0600` and existing files are never
overwritten. The qualification digest is SHA-256 over canonical compact JSON
for the raw evidence structure. The revision is intentionally operator-pinned
to DeepSeek's official model table because `/models` and Chat Completions expose
the stable alias, not a separate immutable revision field. If the official
revision changes, update and review the adapter contract before qualifying it;
passing another revision to this binary fails closed.
