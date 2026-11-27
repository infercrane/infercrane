# Trained-draft proof of concept

Development tooling, not a product path. It trains an EAGLE-3 draft for `Qwen/Qwen3-8B` on chat
prompts from `OpenAssistant/oasst1` and measures it, on one Modal H100, against the published draft
it started from and against no speculation. The product side is `internal/speculator` and the
planner's `speculative_drafts` (see `docs/features/trained-drafts.mdx`).

Rules and pass bar, written before any server: `rules-qwen3-8b-oasst1-2026-11-26.md`.

| File | What |
|---|---|
| `draftlib.py` | Pins, prompt split, request and training-row shapes, the window's order, run summaries, vLLM speculation counters, the training report. Standard library only |
| `trained_draft.py` | The stages (`smoke`, `train`, `measure`, `confirm`), the Modal app (`download`, `gpu_stage`, `launch`), and the laptop commands (`prepare`, `dry-run`, `fetch`). `confirm` measures the trained draft against the control again in a new container, timed runs only, and records the GPU it ran on: the fresh-machine confirmation the judge attaches when the GPUs differ and the gain holds |
| `standin.py` | A stand-in for vLLM's OpenAI server, for the dry run only |
| `judge/` | `go run ./tools/trained-draft/judge RUN_DIR`: the campaign judge (`optimizationcampaign.JudgeRecordedWindow`), the task rule (`qualitycheck.PairedTask`) and the report check (`speculator.DecodeReport`) on a recorded run, and the technique contract's record (`technique.Result`) of the trained draft at every load with its claim. The record carries the `draft-acceptance` evidence as the sha256 of `acceptance.json` (vLLM's speculation counters) and the traffic shape as the sha256 of `traffic-shape.json` (prompt ids and sizes, no text), both written by the measure stage |
| `draft_embedding.py` | Writes the verifier's token embedding into a draft converted from the original EAGLE-3 format, which has none: speculators 0.9.0 with transformers 5.19 would otherwise train it with zero embeddings. Run after every conversion that precedes training |
| `trainer_view.py` | Scores a draft on rows whose hidden states are written, without the trainer's loop: speculators' forward, an independent forward from vLLM's EAGLE-3 math, whether the draft's embedding is the verifier's, and whether the final hidden state predicts the next answer token |
| `test_trained_draft.py` | Offline tests: `python3 -m unittest tools/trained-draft/test_trained_draft.py` |

## Run

```bash
python3 tools/trained-draft/trained_draft.py prepare --run-id RUN       # $0, needs pyarrow
SPECULATORS_BIN=/path/to/venv/bin/speculators python3 tools/trained-draft/trained_draft.py dry-run --run-id RUN   # $0, needs aiohttp and speculators 0.9.0
```

Paid stages go through the coordinator, one at a time, each with its hard cap
(`modal run tools/trained-draft/trained_draft.py::launch --stage STAGE --run-id RUN --budget-usd CAP`).
The first launch builds the image: the pinned vLLM 0.34.0 image plus a separate virtual environment
with speculators 0.9.0 and its own torch.

Output lands on the Modal volume `infercrane-trained-draft` under `runs/RUN/`, and `fetch` copies the
records to `.infercrane/trained-draft/RUN/`. Prompts and the model's answers stay on the volume.
