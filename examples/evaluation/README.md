# External task-quality evaluation

InferCrane does not choose or run an LLM judge. Your CI, Ragas, DeepEval, or
custom evaluator owns the private dataset and full result artifact. It emits
only the aggregate fields in `evaluator-result.json`.

Validate the file against `../../schemas/evaluator-result-v1.schema.json` in
your evaluator pipeline, then bind it to the exact candidate revision:

```bash
infercrane evaluation keygen --file ./quality-evidence.key

infercrane evaluation ingest coder-production REVISION_ID \
  --result ./examples/evaluation/evaluator-result.json \
  --key ./quality-evidence.key \
  --file ./candidate-quality.json \
  --attach

infercrane evaluation verify ./candidate-quality.json
infercrane rollout evaluate coder-production
```

The CLI rejects unknown fields, files larger than 1 MiB, invalid digests,
future timestamps, and prompt/output content. `--attach` is explicit. If the
API attachment fails, the signed local evidence file remains available for a
safe retry with `infercrane evaluation attach`.

Use the same immutable suite and evaluator versions for the active and
candidate revisions. Release Guard returns `WAIT` for incomparable evidence
and never promotes a revision by itself.
