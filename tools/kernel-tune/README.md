# Kernels tuned for your shapes (development harness)

Tunes an engine's own kernels for one GPU and the token counts a workload's steps carry, and
proves the result end to end with the campaign judge. The first operator is vLLM's Triton
fused-MoE kernel; the product side is `internal/kerneltuning` and
`docs/features/kernel-tuning.mdx`.

What it delivers is data, not code: the JSON file of tile settings vLLM already reads from
`VLLM_TUNED_CONFIG_FOLDER`, named after the MoE shape and the GPU. The engine falls back per
file to its shipped file or built-in default, and inside a file every step size where tuning
did not measure better keeps the engine's own default (`kerneltuning.Keys` places keys so
that fallback is exact).

## Files

| File | Runs where | What |
|---|---|---|
| `tune_moe.py` | engine image, on the GPU (`--stand-in` on a laptop) | the tuner: step-size plan, coordinate search, keep rule, reference check, memory floor, hidden sizes, the set and its digest |
| `proof.py` | Modal container (or a laptop for `dry`) | one machine: gap check, tuning, the bracketed window, task-set answers, the window record |
| `loadgen.py` | same | closed-loop load and the window's numbers (the map's measurement) |
| `stand_in_server.py` | laptop | a `vllm serve` stand-in for dry runs: same MoE log lines, streamed answers; not a measurement |
| `kernel_tune_modal.py` | laptop | Modal functions, caps and cancellation, the Go judge |
| `rules-*.md` | | the rules of each paid run, written before its first server |
| `test_kernel_tune.py` | anywhere | offline tests; `go test ./internal/kerneltuning` runs them |

## Run

```bash
export ASDF_GOLANG_VERSION=1.26.6 GOTOOLCHAIN=auto   # this Mac's Go setup; the judge is `go run`
PY=~/.local/share/uv/tools/modal/bin/python           # a Python with the modal client
$PY tools/kernel-tune/kernel_tune_modal.py dry                   # $0: everything with stand-ins
$PY tools/kernel-tune/kernel_tune_modal.py smoke                 # CPU: both images start in Modal (cents)
$PY tools/kernel-tune/kernel_tune_modal.py download              # CPU: stage the pinned weights (30 min at most)
$PY tools/kernel-tune/kernel_tune_modal.py preflight             # CPU: the image's facts (10 min at most)
$PY tools/kernel-tune/kernel_tune_modal.py run --budget-usd 10   # GPU: the proof (paid)
go run ./cmd/infercrane optimize evidence kernel-tuning --file .infercrane/kernel-tune/<run>/window-first.json \
  --fresh .infercrane/kernel-tune/<run>/window-fresh.json --set .infercrane/kernel-tune/<run>/kernel-configs.json
```

A paid run needs its rules file first, a passing `dry`, a passing `smoke`, and a launch by
whoever holds the budget. `run` refuses a budget below its caps. The laptop dry run imports
the modules from this folder, so it cannot show a container that fails at startup in Modal
(8f579b02's download image lacked `proof` and crashed 14 times); `smoke` starts both images
on CPU and imports this file as every function does. Every remote call is cancelled from
the laptop at its own limits: not started within 5 min (CPU) or 20 min (GPU), the first
startup crash Modal reports, or its limit after it started.

## How a machine runs

1. **Gap check** (first machine): one unmeasured start of the untuned engine. Its log names
   the MoE backend and the tuned file it looked for and did not find. Another backend, a
   shipped file, or more than one missing shape ends the machine: nothing to tune.
2. **Tune** that file's shape on this GPU (first machine). No key kept means nothing to measure.
3. **Window**: control, candidate, control, … , control (nine starts, four candidates) on this
   GPU, every cell measured in each start, the task set after starts 1 to 3 (first machine).
   Every start proves from its own log that it read the tuned file (candidate) or fell back
   to the default for it (control).
4. The laptop judges the first window; only a gain the judge calls improved launches the
   fresh machine, which serves the same set unchanged.

The kernel-level numbers in `tuning-report.json` choose what to measure. Only the judge's
decision on the windows is a result.
