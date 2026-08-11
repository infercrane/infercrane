# v2 consolidated manual qualification

Local qualification does not prove real provider behavior. Run the paid suite once after configuring the
provider credentials and serverless template documented in [Release acceptance](/release-acceptance).

```bash
export RUNPOD_KEY_FILE="$HOME/.config/infercrane/runpod-key"
export INFERCRANE_RUNPOD_SERVERLESS_TEMPLATE_ID="YOUR_TEMPLATE_ID"
export INFERCRANE_V2_QUALIFICATION_RUN_ID="$(date -u +%Y%m%dT%H%M%SZ)-v2.0.0-rc.1"

./scripts/qualify-v2-manual.sh run --approve-paid-resources
```

The parent ID creates fixed child identities for general qualification, elastic faults, and serverless faults.
Re-running the same command resumes the persisted stage and child operations instead of submitting a new
intent. Each stage performs guarded cleanup and writes a sanitized report. Inspect progress with:

```bash
./scripts/qualify-v2-manual.sh status
```

If interrupted, rerun `run` with the same ID. For an explicit cleanup sweep:

```bash
./scripts/qualify-v2-manual.sh cleanup
```

Finally verify the provider console shows no run-owned pods or endpoints. Real qualification remains
incomplete until all three reports and the final zero-resource inventory are reviewed.
