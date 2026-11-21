"""How much do H100 machines differ when they serve the same setup? (development only)

The optimization judge compares a candidate with its control while each runs on its own
server, so a fixed speed difference between the two machines looks like a gain or a loss.
Pairing in time cannot remove it, and measuring again on fresh servers only shrinks it
(docs/features/optimization.mdx). This script measures it. Several H100 containers on Modal
each serve the same setup: the Modal PoC's pinned vLLM image and Qwen3-8B weights, with vLLM
defaults, as the PoC's current setup. Each one benchmarks itself with vLLM's own client in
the same container, so no network is in the numbers. Every machine gets the same warmup and
the same measured runs: identical prompts, lengths and concurrency. The spread of the
per-machine means, after run-to-run noise is taken out, is the machine-to-machine difference.

    uv run --no-project --with modal==1.6.4 modal run tools/poc-modal/machine_variance.py \
        --machines 8 --budget-usd 5.25

Spend: each container ends at its function timeout (the hard cap) and serves one call only,
and the run refuses to start when machines x timeout x list rate is above --budget-usd.
Each call is cancelled at its wall-clock limit, so a container that keeps failing while it
starts cannot be retried without end. Modal's billing report is the authority:
`modal billing report --for today --show-resources`.
"""

from __future__ import annotations

import json
import math
import os
import statistics
import subprocess
import sys
import time
import urllib.request
from pathlib import Path

import modal

sys.path.insert(0, str(Path(__file__).resolve().parent))
import modal_gpu  # noqa: E402

SCHEMA = "infercrane.machine-variance/v1"
MODEL = modal_gpu.QWEN3_8B
PORT = 8000
TIMEOUT_SECONDS = 9 * 60  # hard cap per container
QUEUE_SECONDS = 30 * 60  # a call that has no GPU after this long is cancelled (waiting is not billed)
MAX_AT_ONCE = 4
STARTUP_SECONDS = 6 * 60
# The recorded campaign's traffic had 4,813 input and 192 output tokens per request on
# average at 32 concurrent streams. Lengths are fixed here, so every run asks for exactly
# the same work and only the machine can change the result.
INPUT_TOKENS, OUTPUT_TOKENS, CONCURRENCY = 4800, 192, 32
WARMUP_PROMPTS, RUN_PROMPTS = 64, 256
WARMUP_SEED, RUN_SEEDS = 100, (1, 2, 3)
# Higher is better for throughput; lower for the latencies.
METRICS = {"output_throughput": True, "p95_ttft_ms": False, "p95_e2el_ms": False, "mean_tpot_ms": False}

app = modal.App("infercrane-machine-variance")
image = modal_gpu.image.add_local_python_source("modal_gpu")


def _tail(path: Path, lines: int = 40) -> str:
    try:
        return "\n".join(path.read_text(errors="replace").strip().splitlines()[-lines:])
    except OSError:
        return ""


def _healthy() -> bool:
    try:
        with urllib.request.urlopen(f"http://127.0.0.1:{PORT}/health", timeout=2) as response:
            return response.status == 200
    except OSError:
        return False


def host_facts() -> dict:
    """What can make one H100 host serve faster than another, and what tells two hosts apart."""
    fields = "name,uuid,pci.bus_id,driver_version,vbios_version,memory.total,power.limit,clocks.max.sm,clocks.max.mem,ecc.mode.current"
    facts: dict = {}
    probe = subprocess.run(["nvidia-smi", f"--query-gpu={fields}", "--format=csv,noheader"], capture_output=True, text=True)
    if probe.returncode == 0 and probe.stdout.strip():
        facts.update(zip(fields.split(","), (part.strip() for part in probe.stdout.strip().splitlines()[0].split(","))))
    try:
        cpuinfo = Path("/proc/cpuinfo").read_text()
        first = dict(line.split(":", 1) for line in cpuinfo.split("\n\n")[0].splitlines() if ":" in line)
        first = {key.strip(): value.strip() for key, value in first.items()}
        facts["cpu_model"] = first.get("model name") or "unknown"
        facts["cpu_id"] = "/".join(first.get(key, "?") for key in ("vendor_id", "cpu family", "model", "stepping"))
        facts["cpu_threads_visible"] = sum(1 for line in cpuinfo.splitlines() if line.split(":", 1)[0].strip() == "processor")
    except OSError:
        facts["cpu_model"] = "unknown"
    for name in ("MODAL_REGION", "MODAL_CLOUD_PROVIDER"):
        facts[name.lower()] = os.environ.get(name, "unknown")
    return facts


def _gpu_summary(samples: str) -> dict:
    rows = []
    for line in samples.strip().splitlines():
        try:
            rows.append([float(part) for part in line.split(",")])
        except ValueError:
            continue
    if not rows:
        return {}
    columns = list(zip(*rows))
    return {"samples": len(rows), "sm_clock_mhz_mean": round(statistics.fmean(columns[0]), 1), "power_w_mean": round(statistics.fmean(columns[1]), 1),
            "temperature_c_max": max(columns[2]), "utilization_pct_mean": round(statistics.fmean(columns[3]), 1)}


def bench(seed: int, prompts: int) -> dict:
    """One benchmark run against the local server, with the GPU sampled while it runs."""
    sampler = subprocess.Popen(["nvidia-smi", "--query-gpu=clocks.sm,power.draw,temperature.gpu,utilization.gpu", "--format=csv,noheader,nounits", "-lms", "500"],
                               stdout=subprocess.PIPE, stderr=subprocess.DEVNULL, text=True)
    command = ["vllm", "bench", "serve", "--backend", "vllm", "--base-url", f"http://127.0.0.1:{PORT}", "--model", MODEL.id,
               "--tokenizer", MODEL.dir, "--dataset-name", "random", "--random-input-len", str(INPUT_TOKENS),
               "--random-output-len", str(OUTPUT_TOKENS), "--random-range-ratio", "0", "--ignore-eos", "--num-prompts", str(prompts),
               "--max-concurrency", str(CONCURRENCY), "--seed", str(seed), "--percentile-metrics", "ttft,tpot,itl,e2el",
               "--metric-percentiles", "50,95", "--save-result", "--result-dir", "/tmp", "--result-filename", f"bench-{seed}.json", "--disable-tqdm"]
    started = time.monotonic()
    try:
        done = subprocess.run(command, capture_output=True, text=True, env=dict(os.environ, HF_HUB_OFFLINE="1"), timeout=TIMEOUT_SECONDS)
    finally:
        sampler.terminate()
        samples, _ = sampler.communicate(timeout=10)
    if done.returncode != 0:
        raise RuntimeError(f"benchmark seed {seed} exited with {done.returncode}:\n{(done.stdout + done.stderr)[-3000:]}")
    result = json.loads(Path(f"/tmp/bench-{seed}.json").read_text())
    kept = {key: value for key, value in result.items() if not isinstance(value, (list, dict))}
    kept.update(seed=seed, wall_seconds=round(time.monotonic() - started, 1), gpu=_gpu_summary(samples))
    return kept


@app.function(image=image, gpu=modal_gpu.GPU, cpu=modal_gpu.GPU_CPU, memory=modal_gpu.GPU_MEMORY_MIB, volumes={"/weights": modal_gpu.weights},
              timeout=TIMEOUT_SECONDS, retries=0, max_containers=MAX_AT_ONCE, single_use_containers=True)
def measure(machine: int) -> dict:
    started = time.monotonic()
    deadline = started + TIMEOUT_SECONDS - 45
    result: dict = {"machine": machine, "facts": host_facts(), "runs": []}
    print(f"machine {machine}: {json.dumps(result['facts'])}", flush=True)
    log_path = Path("/tmp/server.log")
    with log_path.open("w") as log:
        server = subprocess.Popen(["vllm", "serve", MODEL.dir, "--host", "127.0.0.1", "--port", str(PORT), "--served-model-name", MODEL.id],
                                  stdout=log, stderr=subprocess.STDOUT, env=dict(os.environ, **MODEL.env))
    try:
        ready_by = started + STARTUP_SECONDS
        while not _healthy():
            if server.poll() is not None:
                raise RuntimeError(f"server exited with {server.returncode} while starting")
            if time.monotonic() > ready_by:
                raise TimeoutError(f"server not ready after {STARTUP_SECONDS} s")
            time.sleep(2)
        result["startup_seconds"] = round(time.monotonic() - started, 1)
        result["warmup"] = bench(WARMUP_SEED, WARMUP_PROMPTS)
        expected = result["warmup"]["wall_seconds"] * RUN_PROMPTS / WARMUP_PROMPTS
        for seed in RUN_SEEDS:
            if time.monotonic() + 1.2 * expected > deadline:
                result["stopped_early"] = f"{len(result['runs'])} of {len(RUN_SEEDS)} runs fit in the container's time limit"
                break
            run = bench(seed, RUN_PROMPTS)
            expected = run["wall_seconds"]
            result["runs"].append(run)
            print(f"machine {machine}: seed {seed} {run['output_throughput']:.1f} output tok/s, p95 TTFT {run['p95_ttft_ms']:.0f} ms", flush=True)
    except Exception as failure:  # noqa: BLE001 - recorded in the result, never retried
        result["error"] = f"{type(failure).__name__}: {str(failure)[-3000:]}"
        result["log_tail"] = _tail(log_path)
    finally:
        server.terminate()
        try:
            server.wait(timeout=30)
        except subprocess.TimeoutExpired:
            server.kill()
    result["container_seconds"] = round(time.monotonic() - started, 1)
    return result


def _chi2_quantile(p: float, df: int) -> float:
    """Wilson-Hilferty approximation, within about 1% for df >= 3."""
    z = statistics.NormalDist().inv_cdf(p)
    return df * (1 - 2 / (9 * df) + z * math.sqrt(2 / (9 * df))) ** 3


def spread(machines: list[list[float]]) -> dict | None:
    """Machine-to-machine and run-to-run spread of one metric, as fractions of its level.

    machines holds each machine's run values. On the log scale, the variance of the
    machine means is the machine variance plus the run variance over the runs per
    machine; the run variance comes from the runs within each machine."""
    machines = [runs for runs in machines if len(runs) >= 2]
    if len(machines) < 3:
        return None
    logs = [[math.log(value) for value in runs] for runs in machines]
    means = [statistics.fmean(runs) for runs in logs]
    within_df = sum(len(runs) - 1 for runs in logs)
    within = sum((value - mean) ** 2 for runs, mean in zip(logs, means) for value in runs) / within_df
    per_machine = statistics.harmonic_mean([len(runs) for runs in logs])
    of_means = statistics.variance(means)
    machine = max(0.0, of_means - within / per_machine)
    # One-sided 90% upper bound from the variance of the means alone, which still holds
    # run noise, so it can only overstate the machine spread.
    upper = math.sqrt((len(means) - 1) * of_means / _chi2_quantile(0.10, len(means) - 1))
    return {"machines": len(means), "runs": sum(len(runs) for runs in logs), "run_to_run": math.sqrt(within), "machine_sd": math.sqrt(machine),
            "machine_sd_upper_90": upper, "two_machines_differ_sd": math.sqrt(2 * machine), "two_machines_differ_sd_upper_90": math.sqrt(2) * upper,
            "max_over_min": math.exp(max(means) - min(means)) - 1}


def analyze(results: list[dict]) -> dict:
    usable = [result for result in results if len(result.get("runs", [])) >= 2]
    groups = {"all": usable}
    drivers = sorted({result["facts"].get("driver_version", "unknown") for result in usable})
    if len(drivers) > 1:
        groups.update({f"driver {driver}": [result for result in usable if result["facts"].get("driver_version", "unknown") == driver] for driver in drivers})
    uuids = [result["facts"].get("uuid") for result in usable]
    return {"usable_machines": len(usable), "failed_machines": len(results) - len(usable), "drivers": drivers,
            "repeated_gpus": len(uuids) - len(set(uuids)), "cpu_models": sorted({result["facts"].get("cpu_model", "unknown") for result in usable}),
            "metrics": {group: {metric: spread([[run[metric] for run in result["runs"]] for result in members]) for metric in METRICS}
                        for group, members in groups.items()}}


def rate_per_second() -> float:
    from broker import CORE_PER_SECOND, GIB_PER_SECOND, H100_PER_SECOND

    return H100_PER_SECOND + modal_gpu.GPU_CPU * CORE_PER_SECOND + modal_gpu.GPU_MEMORY_MIB / 1024 * GIB_PER_SECOND


@app.local_entrypoint()
def main(machines: int = 8, budget_usd: float = 0.0, stagger_seconds: int = 20, out: str = ".infercrane/poc-modal/output") -> None:
    rate = rate_per_second()
    cap = machines * TIMEOUT_SECONDS * rate
    if cap > budget_usd:
        raise SystemExit(f"{machines} machines can cost up to ${cap:.2f} ({TIMEOUT_SECONDS // 60} min each at ${rate * 3600:.2f}/h); pass --budget-usd {cap:.2f} or fewer machines")
    print(f"{machines} H100 containers, at most {MAX_AT_ONCE} at once, {TIMEOUT_SECONDS // 60} min each: cap ${cap:.2f} at list prices", flush=True)
    calls = []
    for machine in range(machines):
        if machine:
            time.sleep(stagger_seconds)
        calls.append((machine, measure.spawn(machine), time.time()))
    results = []
    try:
        for machine, call, spawned in calls:
            limit = spawned + QUEUE_SECONDS + TIMEOUT_SECONDS + 60 - time.time()
            try:
                results.append(call.get(timeout=max(5.0, limit)))
            except Exception as failure:  # noqa: BLE001 - one lost machine does not void the others
                results.append({"machine": machine, "runs": [], "error": f"{type(failure).__name__}: {str(failure)[-500:]}"})
    finally:
        for _, call, _ in calls:
            call.cancel(terminate_containers=True)
    report = {"schema_version": SCHEMA, "measured_at": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
              "setup": {"model": MODEL.id, "model_revision": MODEL.revision, "runtime_image": modal_gpu.VLLM_IMAGE,
                        "vllm_version": modal_gpu.VLLM_VERSION, "server_args": ["--served-model-name", MODEL.id], "server_env": MODEL.env,
                        "gpu": modal_gpu.GPU, "cpu": modal_gpu.GPU_CPU, "memory_mib": modal_gpu.GPU_MEMORY_MIB},
              "workload": {"client": "vllm bench serve (same container)", "dataset": "random", "input_tokens": INPUT_TOKENS, "output_tokens": OUTPUT_TOKENS,
                           "ignore_eos": True, "concurrency": CONCURRENCY, "warmup_prompts": WARMUP_PROMPTS, "run_prompts": RUN_PROMPTS,
                           "warmup_seed": WARMUP_SEED, "run_seeds": list(RUN_SEEDS)},
              "spend": {"cap_usd": round(cap, 2), "rate_usd_per_hour": round(rate * 3600, 4),
                        "estimated_usd": round(sum(result.get("container_seconds", 0) for result in results) * rate, 2),
                        "basis": "container seconds the containers measured themselves x Modal list price; the billing report is the authority"},
              "results": results, "analysis": analyze(results)}
    path = Path(out) / f"machine-variance-{report['measured_at'].replace(':', '')}.json"
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(json.dumps(report, indent=2) + "\n")
    print(json.dumps(report["analysis"], indent=2))
    print(f"wrote {path}; estimated ${report['spend']['estimated_usd']:.2f}")
