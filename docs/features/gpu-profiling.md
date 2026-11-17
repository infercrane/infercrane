---
title: GPU profiling
description: Capture a short trace of a serving replica, see where GPU time goes, and compare captures before and after a change.
---

# GPU profiling

A GPU profile answers one question first: what holds this endpoint's GPUs back? `infercrane profile`
records a bounded trace through the serving engine's own profiler, analyzes it, and leads with a
one-sentence verdict that cites the measured values. The numbers behind it, a timeline, a flame
graph, and a before-and-after comparison sit underneath, and the diagnosis feeds
[Optimize](/features/optimization#choose-setups-from-the-endpoint-diagnosis) so a fix can be tested on your
traffic.

```text title="infercrane profile coder, abridged (sample trace: generated, not a measurement)"
GPU profile · coder · torch-profiler vllm-0.22.1 · NVIDIA H100 80GB HBM3

The GPU sat idle 36% of the capture, mostly waiting for the host to launch work.
idle 36% of 148 ms · 97% of idle waiting on launches · launch delay p50 3 µs

  GPU busy             64% (compute 64% · memory and communication only 0.1%)
  GPU idle             36% · waiting on the host 97% · launch latency 2.7% · other 0%
  Launches             376 (40 graph) · 2536/s · 64% shorter on the GPU than their launch call
  Communication        not applicable: No collective communication kernels ran, so there is nothing to overlap.
  Steps                42 · p50 3.29 ms · p95 3.29 ms (from ProfilerStep#0 annotations)
  Prefill / decode     14% / 86% of kernel time · mixed 0% · unknown 0% (from attention kernel names)
  Roofline             unknown: Only 15% of matrix-multiply time had recorded shapes, too little to place the workload.
```

## Capture a profile

The engine writes its trace to its own disk, so run the command where both the engine's HTTP port and
its trace directory are reachable: on the GPU host, or in a sidecar container that mounts the same
trace volume.

```bash
infercrane profile coder \
  --engine-url http://127.0.0.1:8000 \
  --trace-dir /profiles \
  --duration 10s
```

The engine needs its profiler turned on when it starts:

| Engine | Start the server with |
|---|---|
| vLLM 0.13 or later | `--profiler-config '{"profiler":"torch","torch_profiler_dir":"/profiles","torch_profiler_record_shapes":true,"torch_profiler_with_stack":false}'` |
| SGLang | nothing; `/start_profile` is always served. InferCrane sends `output_dir`, `activities: ["CPU","GPU"]`, and `record_shapes: true`. |

The command detects the engine (SGLang answers `/get_server_info`, vLLM `/version`), asks before it
starts, calls `/start_profile`, waits for `--duration` (2 to 30 seconds), calls `/stop_profile`,
waits up to `--flush-timeout` for the engine to finish writing, analyzes the trace locally, and records
the result against the deployment's active revision. `--deployment` names the deployment when it is
not called like the endpoint. `--engine-key-env NAME` sends a bearer key from that environment
variable when the engine requires one.

### What a capture costs

While it records, the engine traces every kernel launch with the PyTorch profiler (Kineto on CUPTI or
ROCm), so that replica serves more slowly for the duration. InferCrane does not measure the slowdown
and makes no claim about its size. The command states this and asks before starting; pass `--yes` in
scripts. A capture is bounded to 30 seconds, and the profiler is stopped even when the command is
interrupted. Capture on one replica, at a time when slower requests for a few seconds are acceptable.

### What is recorded

A capture never contains request content. The parser reads only kernel, operator, and runtime-call
names with their times, devices, streams, and correlation IDs, plus the input shapes of matrix
multiplies. Python stack frames, operator arguments such as concrete inputs, and every
`record_function` annotation other than the engine-step markers are skipped while the file is read.
The full trace stays where the engine wrote it; InferCrane stores the analysis, the flame-graph stacks,
and an excerpt of the busiest second (at most 20,000 events).

### Hardware counters during the capture

`--dcgm-url http://127.0.0.1:9400/metrics` samples [DCGM Exporter](#continuous-signals) once a second
while the trace records and keeps SM active, SM occupancy, tensor active, DRAM active, PCIe, and
NVLink throughput beside the analysis. A field the exporter does not send stays unknown.

## Analyze traces you already have

```bash
infercrane profile coder --trace ./profiles                 # Kineto JSON or .json.gz, one file per rank
infercrane profile coder --trace ./rocprof --format rocprofv3
infercrane profile coder --trace ./nsys --no-upload
```

| Source | How to produce it |
|---|---|
| PyTorch profiler (Kineto), NVIDIA or AMD | vLLM or SGLang `/start_profile` and `/stop_profile`, or `torch.profiler` with `record_shapes=True` |
| rocprofv3 | `rocprofv3 --kernel-trace --hip-runtime-trace --memory-copy-trace --output-format csv -- …`, or `rocpd convert -f csv` |
| Nsight Systems (optional) | `nsys stats --report cuda_gpu_trace,cuda_api_trace --format csv --output report report.nsys-rep` |

Columns are read by name, so added columns in newer tool versions are ignored. `--no-upload` analyzes
without signing in or recording anything. `--accelerator NAME` names the GPU when the trace does not,
and `--peak-tflops` with `--peak-tbps` set the roofline peaks for a GPU that has no listed datasheet
peak.

## What the analysis reports

The analysis is `gpu-trace-analysis-v1`. Its definitions follow
[Holistic Trace Analysis](https://github.com/facebookresearch/HolisticTraceAnalysis) (MIT),
re-implemented in Go so the CLI needs no Python. The window runs from the first to the last GPU
activity. Every share is a fraction of GPU time (window × GPUs) unless it says otherwise. A value the
trace cannot support is `null` and its section's `reason` says why; it is never shown as zero.

| Section | What it measures |
|---|---|
| Utilization | Busy time is the union of kernel and memory-operation intervals per GPU, so concurrent streams count once. Compute is the union of non-communication, non-memory kernels; non-compute is busy time with only communication or memory work. A busy series per bucket (about 200 per capture). |
| Idle causes | Each idle gap between kernels on a GPU is **host wait** when the launch of the work that ended it was still running, or had not started, when the GPU went idle; **launch latency** when the work was already queued and the gap is under 30 µs; **other** otherwise (dependencies, synchronization). Idle time before a GPU's first or after its last activity is other. Gap count, p50, p95, longest, and the ten longest gaps. |
| Launches | Kernel and graph launch calls, launches per second, the busiest host thread's share of time inside launch calls, launch delay (kernel start minus launch-call start; for a CUDA graph, its first kernel), and the share of launches whose GPU work is shorter than the launch call. |
| Kernels | Kernel time by category (attention, gemm, communication, memory, norm, activation, sampling, other) and the 25 kernels with the most time. |
| Overlap | The share of collective-communication time that ran while a compute kernel ran on the same GPU. |
| Steps | Engine steps from step annotations (`ProfilerStep#N`, `execute_model`, and similar) or, without them, from GPU idle gaps of 100 µs or more on the busiest GPU. Count, p50, p95, and up to 1,000 steps with their GPU busy time. |
| Prefill and decode | Kernel time by step phase. A step is prefill or decode when its annotation says so or when its attention kernels name one phase only (FlashInfer `BatchPrefill`/`BatchDecode`, paged-attention, TensorRT-LLM XQA); both is mixed; neither is unknown. |
| Roofline | For each 16-bit matrix-multiply shape with recorded input shapes: FLOPs (2·M·N·K), the minimum bytes moved, arithmetic intensity, and achieved TFLOPS, against the GPU's datasheet peak and ridge point. A position is named only when at least half of matrix-multiply time has shapes; CUDA-graph replays never record them. |

### Verdict rules

The first rule that fires names the bottleneck; every fired rule is listed as a finding with its
measured value and threshold.

| Rule | Fires when | Bottleneck |
|---|---|---|
| `gpu.idle_host_bound` | idle time waiting on the host ≥ 10% of GPU time | `host-launch` |
| `gpu.launch_bound_kernels` | ≥ 50% of launches finish on the GPU faster than their launch call | `host-launch` |
| `gpu.communication_exposed` | communication not under compute ≥ 10% of GPU time | `communication` |
| `gpu.memory_bound_gemm` | ≥ 60% of shaped matrix-multiply time below the ridge point | `memory-bandwidth` |
| `gpu.compute_bound_gemm` | ≥ 60% of shaped matrix-multiply time above the ridge point | `compute` |
| `gpu.attention_heavy` | attention ≥ 35% of kernel time | `attention` |
| `gpu.idle` | idle ≥ 20% of GPU time | finding only |
| `gpu.long_bubbles` | idle gap p95 ≥ 1 ms | finding only |
| `gpu.decode_dominant` | decode ≥ 70% of kernel time | finding only |

With no rule naming a bottleneck, a GPU busy at least 80% of the time reads as `busy`; otherwise the
verdict is `unknown`, and says so when the trace has no launch calls to explain idle time.

### Roofline peaks

Peaks are vendor datasheet figures (dense, without sparsity), not measurements: H100 SXM and PCIe,
H100 NVL, H200 and H200 NVL, A100 40GB and 80GB, L40S, L4, A10, GeForce RTX 4090, and AMD Instinct
MI300X and MI325X. Any other accelerator leaves the roofline unknown until `--peak-tflops` and
`--peak-tbps` declare one.

## Read and compare captures

```bash
infercrane profile coder                         # newest capture of the serving revision
infercrane profile coder --list                  # every retained capture, all revisions
infercrane profile coder --capture gpc-…         # one capture
infercrane profile coder --compare gpc-BEFORE    # against the newest other capture
infercrane profile coder --compare gpc-BEFORE,gpc-AFTER --output json
```

A comparison puts two analyzed captures side by side, for example the starting point and the
revision an optimization run rolled out: GPU busy and idle, idle waiting on the host, short launches,
launch delay, communication under compute, step p50 and p95, decode share, and memory-bound share.
Kernel time is compared per second of capture, so captures of different lengths compare. A change of
two points or less on a share (two percent on other values) is reported as no clear change, and a
value that one side did not measure stays unknown. Different accelerators, GPU counts, or profilers are
named as warnings.

`--open` opens the capture in the dashboard: Observe › Profiling leads with the verdict, and with
**Advanced** on it shows the utilization and roofline charts, the top kernels, a zoomable timeline of
the busiest second with an **Open in Perfetto UI** button, the flame graph, and the comparison. The
trace files are standard Kineto JSON, so they also open directly in [Perfetto UI](https://ui.perfetto.dev).

### JSON output

`--output json` is stable. A capture, trace analysis, or stored read prints
`infercrane.profile-result/v1`:

| Field | Meaning |
|---|---|
| `schema_version` | `infercrane.profile-result/v1` |
| `endpoint`, `deployment`, `revision` | What the capture belongs to; `deployment` and `revision` are null when not recorded |
| `source` | `capture`, `trace_files`, or `stored` |
| `capture_id`, `uploaded`, `dashboard_url` | The recorded capture, or null with `uploaded: false` |
| `trace_files` | Local trace files read (empty for a stored read) |
| `capture` | `tool`, `tool_version`, `accelerator`, `gpus`, `started_at`, `ended_at` |
| `analysis` | The `gpu-trace-analysis-v1` analysis, or null for a call-stack-only capture, with `reason` |

`--list` prints `infercrane.profile-list/v1` and `--compare` prints `infercrane.profile-comparison/v1`
with the API's comparison fields.

## API

| Method and path | Permission | Returns |
|---|---|---|
| `POST /api/v1/deployments/{name}/gpu-profiles` | deploy | Records one `infercrane.gpu-profile/v1` or `/v2` capture against the active revision |
| `GET /api/v1/endpoints/{name}/gpu-profile` | read | The newest capture of the serving revision |
| `GET /api/v1/endpoints/{name}/gpu-profiles` | read | Every retained capture behind the endpoint, newest first, with `active`, the verdict, and GPU busy |
| `GET /api/v1/endpoints/{name}/gpu-profiles/{id}` | read | One retained capture, including revisions that no longer serve |
| `GET /api/v1/endpoints/{name}/gpu-profiles/compare?base=ID&head=ID` | read | The comparison; `422 gpu_profile_not_comparable` when either side has no analysis |

A v2 recording is a [v1 recording](/features/telemetry#gpu-profiles) plus `analysis` (required) and
`timeline_excerpt` (optional). The control plane validates every bound and range: shares in 0..1,
busy plus idle equal to the window, unknown values with a reason, at most 25 top kernels, 1,000 steps,
50 roofline points, and 20,000 excerpt events within 2 seconds, and an analysis window that fits the
capture. It cannot re-run the analysis, so a recording, like a DCGM snapshot, is trusted to the
deploy permission of whoever posts it.

## Storage and retention

The recording is one JSONB document in `gpu_profiles` beside its capture's tenant, deployment, revision,
tool, and times. The newest eight captures per deployment are kept, and captures that ended more than
30 days ago are removed when the next one is recorded and are never read. Nothing is read on the
inference path.

## Continuous signals

Profiling traces are short and opt-in. For always-on, low-overhead counters, run
`infercrane telemetry collect dcgm DEPLOYMENT --url … --interval 15s` beside
[dcgm-exporter](/features/telemetry#nvidia-gpu-evidence): it posts SM active, SM occupancy, tensor
active, DRAM active, PCIe, and NVLink readings every interval, each fresh for three intervals, and the
readiness rules read them continuously.

Continuous kernel-level profiling with eBPF and CUPTI
([Parca Agent with parcagpu](https://www.polarsignals.com/blog/posts/2025/10/22/gpu-profiling),
Apache-2.0) was evaluated and is not integrated: it needs a privileged eBPF DaemonSet on every node and
`CUDA_INJECTION64_PATH` in the serving container, which fits operator-owned Kubernetes but not RunPod,
Vast, or serverless replicas. Its pprof output could become a v1 recording later. Commercial continuous
profilers such as Zymtrace stay an integration option rather than something InferCrane rebuilds.
Licences of every tool involved are on [Dependencies](/dependencies).

## Sample traces

`internal/gputrace/testdata` holds small traces generated by `internal/testtools/tracefixture` in the
shapes vLLM and SGLang (Kineto), rocprofv3, and Nsight Systems write, with golden analyses. They are
generated samples: kernel names follow the upstream libraries, every time is made up, and none of their
numbers is a measurement. Regenerate them with
`go test ./internal/gputrace -run TestSample -update`.

## Not yet qualified

The capture path is tested against fake vLLM and SGLang servers, and the parsers against generated
traces. It has not been run against a live engine on a GPU, and the rocprofv3 and Nsight Systems
column names come from their documentation rather than a live install.
