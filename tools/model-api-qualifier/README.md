# Model API supplier qualifier

This operator-only tool qualifies one explicit supplier contract through the
same strict adapter used by the production gateway. It runs bounded buffered
and streaming Chat Completions samples, verifies complete usage and terminal
SSE framing, and emits append-only, secret-free evidence.

## Profiles

| `--profile` | Adapter | Exact supplier model | Credential | Expected revision |
|---|---|---|---|---|
| `deepseek-v4-flash` | `deepseek-openai` | `deepseek-v4-flash` | `DEEPSEEK_API_KEY` | `DeepSeek-V4-Flash-0731` |
| `glm-5.2` | `zai-openai` | `glm-5.2` | `ZAI_API_KEY` | `glm-5.2` |
| `glm-5.3` | `zai-openai` | `glm-5.3` | `ZAI_API_KEY` | `glm-5.3` |
| `glm-5.3-flash` | `zai-openai` | `glm-5.3-flash` | `ZAI_API_KEY` | `glm-5.3-flash` |
| `qwen3.8-27b-runpod` | `runpod-sglang-openai-lb` | `Qwen/Qwen3.8-27B-FP8` | `RUNPOD_API_KEY` | `017b9c7af6b5689d5dd426a76e0bc077eb5ca20a` |

The DeepSeek profile remains the default for compatibility with existing
operator scripts. `--confirm-live-deepseek` remains accepted as a deprecated
alias, but new automation must use the provider-neutral `--confirm-live`.

Every live run is billable. Use dedicated, capped supplier credentials and an
operator-controlled machine. Credentials are copied only inside the adapter
boundary and are never written to evidence or included in errors.

## Hosted API example

The following example qualifies GLM-5.3. Change the profile, credential, tuple,
revision, and artifact paths together; never reuse evidence across products.

```sh
ZAI_API_KEY='replace-in-shell-only' \
go run ./tools/model-api-qualifier \
  --profile glm-5.3 \
  --confirm-live \
  --offer-id zai-glm-5-3 \
  --offer-version 1 \
  --qualification-id zai-glm-5-3-q-20260903 \
  --tuple-key 'zai|glm-5.3|openai|global' \
  --expected-revision glm-5.3 \
  --samples-per-mode 3 \
  --max-output-tokens 512 \
  --request-timeout 180s \
  --total-timeout 10m \
  --valid-for 1h \
  --evidence-ref 's3://infercrane-qualification/zai/glm-5.3/2026-09-03.json' \
  --evidence-output ./artifacts/zai-glm-5.3-raw.json \
  --qualification-output ./artifacts/zai-glm-5.3-qualification.json
```

DeepSeek uses the same flags with `--profile deepseek-v4-flash`,
`DEEPSEEK_API_KEY`, and revision `DeepSeek-V4-Flash-0731`.

## RunPod load-balanced example

RunPod qualification is bound to an exact load-balanced endpoint origin and
region. Queue URLs, paths, query strings, custom ports, nested subdomains, and
non-RunPod origins fail before a request is sent. The endpoint must serve the
pinned Qwen checkpoint through the production SGLang adapter.

```sh
RUNPOD_API_KEY='replace-in-shell-only' \
go run ./tools/model-api-qualifier \
  --profile qwen3.8-27b-runpod \
  --endpoint-origin 'https://YOUR_ENDPOINT_ID.api.runpod.ai' \
  --region 'EU-NL-1' \
  --confirm-live \
  --offer-id runpod-qwen38-sglang \
  --offer-version 1 \
  --qualification-id runpod-qwen38-sglang-q-20260903 \
  --tuple-key 'runpod|Qwen/Qwen3.8-27B-FP8|openai|EU-NL-1' \
  --expected-revision 017b9c7af6b5689d5dd426a76e0bc077eb5ca20a \
  --samples-per-mode 3 \
  --max-output-tokens 512 \
  --request-timeout 180s \
  --total-timeout 10m \
  --valid-for 1h \
  --evidence-ref 's3://infercrane-qualification/runpod/qwen38/2026-09-03.json' \
  --evidence-output ./artifacts/runpod-qwen38-raw.json \
  --qualification-output ./artifacts/runpod-qwen38-qualification.json
```

The raw artifact records only a SHA-256 digest of the target origin, not the
origin itself. This binds the measurement to its target without publishing
private routing information.

Upload the qualification manifest with the operator CLI:

```sh
infercrane model-api publish qualification \
  --file ./artifacts/zai-glm-5.3-qualification.json
```

Both artifacts are created with mode `0600`; existing files are never
overwritten. The qualification digest is SHA-256 over canonical compact JSON
for the raw evidence structure. Supplier API aliases do not prove immutable
weights, so the DeepSeek and Z.ai revision fields are explicit operator pins.
The RunPod profile is instead pinned to the immutable Hugging Face checkpoint
listed above and must be updated only with a reviewed adapter and deployment
recipe change.
