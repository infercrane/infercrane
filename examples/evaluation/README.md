# External task-quality evaluation

InferCrane does not choose or run an LLM judge. Your CI, Ragas, DeepEval, or
custom evaluator owns the private dataset and full result artifact. It emits
only the aggregate fields in `evaluator-result.json`.

Validate the file against `../../schemas/evaluator-result-v2.schema.json` in
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

## Eval sets for the built-in answer check

`eval-set-tools.json` is a `quality-eval-set/v2` file: a plain prompt, two
cases that declare function tools, and a conversation that carries an earlier
call and its result. Tool cases are scored on the calls, and a candidate that
breaks a call the reference made correctly is blocked.

```bash
infercrane evaluation set digest ./examples/evaluation/eval-set-tools.json
infercrane evaluation set upload ./examples/evaluation/eval-set-tools.json
infercrane optimize create MODEL --provider PROVIDER --gpu GPU --eval-set sha256:DIGEST
```
