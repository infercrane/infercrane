#!/usr/bin/env python3
"""Rehearse an optimization campaign on a laptop, without a datacenter GPU.

Serves a small model with llama.cpp, measures a control and a candidate recipe
with AIPerf as control, candidate, control, checks deterministic output parity,
writes InferCrane optimization evidence, and runs the real evaluator on it.

This proves the decision path with real measurements. The numbers come from a
laptop, so they are never evidence about a datacenter GPU.
"""

from __future__ import annotations

import argparse
import hashlib
import json
import platform
import shutil
import socket
import subprocess
import sys
import tempfile
import time
import urllib.error
import urllib.request
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]
CANDIDATE_ARGS = ["--flash-attn", "on", "--cache-type-k", "q8_0", "--cache-type-v", "q8_0"]
PARITY_PROMPTS = (
    "List three prime numbers.",
    "Name the capital of France in one word.",
    "Write one short sentence about rivers.",
)
SLO = {"max_ttft_ms": 5000.0, "max_itl_ms": 100.0}


def digest(value: object) -> str:
    return "sha256:" + hashlib.sha256(json.dumps(value, sort_keys=True).encode()).hexdigest()


def request_json(url: str, body: dict | None = None, timeout: float = 120) -> dict:
    data = json.dumps(body).encode() if body is not None else None
    request = urllib.request.Request(url, data=data, headers={"Content-Type": "application/json"})
    with urllib.request.urlopen(request, timeout=timeout) as response:
        return json.loads(response.read())


class Server:
    def __init__(self, model: str, port: int, parallel: int, extra: list[str], log: Path):
        self.base = f"http://127.0.0.1:{port}"
        with socket.socket() as probe:  # A leftover server would be measured instead of this arm.
            if probe.connect_ex(("127.0.0.1", port)) == 0:
                raise SystemExit(f"port {port} is already in use; stop the process on it or pass --port")
        self.log = log.open("w")
        command = [shutil.which("llama-server") or "llama-server", "-hf", model, "--host", "127.0.0.1", "--port", str(port), "--ctx-size", "8192", "--parallel", str(parallel), "--jinja", *extra]
        self.process = subprocess.Popen(command, stdout=self.log, stderr=subprocess.STDOUT)

    def wait(self, timeout: float = 600) -> None:
        deadline = time.monotonic() + timeout
        while time.monotonic() < deadline:
            if self.process.poll() is not None:
                raise RuntimeError(f"llama-server exited; see {self.log.name}")
            try:
                if request_json(self.base + "/health", timeout=2).get("status") == "ok":
                    return
            except (urllib.error.URLError, OSError, json.JSONDecodeError):
                pass
            time.sleep(1)
        raise RuntimeError("llama-server did not become ready")

    def stop(self) -> None:
        self.process.terminate()
        try:
            self.process.wait(timeout=30)
        except subprocess.TimeoutExpired:
            self.process.kill()
        self.log.close()


def parity_outputs(server: Server) -> list[str]:
    outputs = []
    for prompt in PARITY_PROMPTS:
        body = {"model": "local", "temperature": 0, "seed": 17, "max_tokens": 48, "messages": [{"role": "user", "content": prompt}]}
        outputs.append(request_json(server.base + "/v1/chat/completions", body)["choices"][0]["message"]["content"])
    return outputs


def models_ok(server: Server) -> bool:
    try:
        return bool(request_json(server.base + "/v1/models").get("data"))
    except (urllib.error.URLError, OSError, json.JSONDecodeError):
        return False


def measure_lane(server: Server, args: argparse.Namespace, concurrency: int, directory: Path) -> dict:
    command = [
        args.aiperf, "profile", "--model", args.tokenizer, "--url", server.base, "--endpoint-type", "chat", "--streaming",
        "--tokenizer", args.tokenizer, "--request-count", str(args.requests), "--concurrency", str(concurrency),
        "--synthetic-input-tokens-mean", str(args.input_tokens), "--synthetic-input-tokens-stddev", "0",
        "--output-tokens-mean", str(args.output_tokens), "--output-tokens-stddev", "0", "--random-seed", "17",
        "--ui", "none", "--artifact-dir", str(directory),
        # llama-server has no /metrics endpoint by default and a laptop has no
        # DCGM or NVML. AIPerf's collectors for both then miss heartbeats and
        # turn a complete run into exit status 1.
        "--no-server-metrics", "--no-gpu-telemetry",
    ]
    output = directory.parent / f"{directory.name}-aiperf.txt"
    for attempt in (1, 2):
        shutil.rmtree(directory, ignore_errors=True)
        with output.open("w") as log:
            completed = subprocess.run(command, stdout=log, stderr=subprocess.STDOUT)
        if completed.returncode == 0:
            break
        print(f"   AIPerf exited {completed.returncode} on attempt {attempt}; output: {output}", flush=True)
    else:
        tail = "\n".join(output.read_text(errors="replace").splitlines()[-15:])
        raise SystemExit(f"AIPerf failed twice for {directory.name}. Last output lines:\n{tail}\nFull output: {output}")
    summary = json.loads((directory / "profile_export_aiperf.json").read_text())
    records = [json.loads(line) for line in (directory / "profile_export.jsonl").read_text().splitlines() if line.strip()]
    succeeded = [record for record in records if not record.get("error") and "time_to_first_token" in record.get("metrics", {})]

    def value(record: dict, name: str) -> float:
        return float(record["metrics"].get(name, {}).get("value", 0.0))

    within = [record for record in succeeded if value(record, "time_to_first_token") <= SLO["max_ttft_ms"] and value(record, "inter_token_latency") <= SLO["max_itl_ms"]]
    attainment = len(within) / len(records) if records else 0.0
    mismatched = [record for record in succeeded if int(value(record, "input_sequence_length")) != args.input_tokens]
    duration = float(summary["benchmark_duration"]["avg"])
    output_rate = float(summary["output_token_throughput"]["avg"])
    request_rate = float(summary["request_throughput"]["avg"])
    return {
        "lane": {
            "concurrency": concurrency, "requests": len(records), "successful_requests": len(succeeded), "independent_runs": 1,
            "measurement_seconds": duration, "slo_attainment": attainment,
            "ttft_p95_ms": float(summary["time_to_first_token"]["p95"]), "itl_p95_ms": float(summary["inter_token_latency"]["p95"]),
            "aggregate_output_tokens_per_second": output_rate, "request_throughput_requests_per_second": request_rate,
            "slo_goodput_requests_per_second": request_rate * attainment, "slo_qualified_output_tokens_per_second": output_rate * attainment,
            "gpu_seconds_per_successful_request": duration / max(1, len(succeeded)), "cost_per_1m_successful_output_tokens_usd": 0.0,
        },
        "errors": len(records) - len(succeeded),
        "mismatched": len(mismatched),
    }


def run_arm(name: str, extra: list[str], args: argparse.Namespace, work: Path) -> dict:
    print(f"== {name}: llama-server {' '.join(extra) or '(defaults)'}", flush=True)
    server = Server(args.model, args.port, max(args.lanes), extra, work / f"{name}.log")
    try:
        server.wait()
        models = models_ok(server)
        outputs = parity_outputs(server)
        lanes = [measure_lane(server, args, concurrency, work / f"{name}-c{concurrency}") for concurrency in args.lanes]
    finally:
        server.stop()
    for result in lanes:
        lane = result["lane"]
        print(f"   c{lane['concurrency']}: {lane['aggregate_output_tokens_per_second']:.1f} tok/s, p95 TTFT {lane['ttft_p95_ms']:.0f} ms, SLO {lane['slo_attainment']:.0%}", flush=True)
    return {"name": name, "args": extra, "models": models, "outputs": outputs, "lanes": lanes}


def candidate(arm: dict, workload_digest: str, parity: bool) -> dict:
    requests = sum(result["lane"]["requests"] for result in arm["lanes"])
    errors = sum(result["errors"] for result in arm["lanes"])
    mismatched = sum(result["mismatched"] for result in arm["lanes"])
    return {
        "id": arm["name"], "runtime_id": "llama.cpp", "origin": "infercrane_generated", "evidence_level": "screening",
        "evidence_state": "reproduced", "recipe_digest": digest(arm["args"]), "workload_digest": workload_digest,
        "quality_passed": arm["models"] and parity, "quality_gates": [{"name": "api_models", "passed": arm["models"]}, {"name": "runtime_parity", "passed": parity}],
        "error_rate": errors / requests if requests else 1.0, "prompt_token_mismatch_rate": mismatched / requests if requests else 1.0,
        "lanes": [result["lane"] for result in arm["lanes"]],
    }


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("--model", default="Qwen/Qwen3-0.6B-GGUF:Q8_0", help="llama.cpp Hugging Face model reference")
    parser.add_argument("--tokenizer", default="Qwen/Qwen3-0.6B")
    parser.add_argument("--candidate-args", default=json.dumps(CANDIDATE_ARGS), help="JSON list of llama-server arguments for the candidate")
    parser.add_argument("--lanes", default="1,4", help="comma-separated concurrency lanes")
    parser.add_argument("--requests", type=int, default=16, help="requests per lane, at least 12")
    parser.add_argument("--input-tokens", type=int, default=256)
    parser.add_argument("--output-tokens", type=int, default=64)
    parser.add_argument("--port", type=int, default=18091)
    parser.add_argument("--aiperf", default=shutil.which("aiperf") or "aiperf")
    parser.add_argument("--output-dir", type=Path, default=None)
    args = parser.parse_args()
    args.lanes = [int(value) for value in args.lanes.split(",")]
    if args.requests < 12:
        parser.error("screening evidence needs at least 12 requests per lane")
    work = args.output_dir or Path(tempfile.mkdtemp(prefix="infercrane-rehearsal-"))
    work.mkdir(parents=True, exist_ok=True)

    candidate_args = json.loads(args.candidate_args)
    control_a = run_arm("control", [], args, work)
    trial = run_arm("candidate", candidate_args, args, work)
    control_b = run_arm("control-repeat", [], args, work)

    drift = max(
        abs(a["lane"]["aggregate_output_tokens_per_second"] - b["lane"]["aggregate_output_tokens_per_second"]) / a["lane"]["aggregate_output_tokens_per_second"]
        for a, b in zip(control_a["lanes"], control_b["lanes"])
    )
    workload = {"input_tokens": args.input_tokens, "output_tokens": args.output_tokens, "lanes": args.lanes, "requests": args.requests, "seed": 17}
    workload_digest = digest(workload)
    evidence = {
        "schema_version": "infercrane.dev/optimization-evidence/v1",
        "model_identity": args.model,
        "hardware_identity": f"laptop:{platform.system()}-{platform.machine()}:llama.cpp",
        "workload_id": "laptop-rehearsal-synthetic",
        "workload_digest": workload_digest,
        "required_concurrency_lanes": args.lanes,
        "slo": {"scope": "each_concurrency_lane", "max_ttft_ms": SLO["max_ttft_ms"], "max_itl_ms": SLO["max_itl_ms"], "min_successful_fraction": 0.99,
                "max_error_rate": 0.01, "max_prompt_token_mismatch_rate": 0.0, "required_quality_gates": ["api_models", "runtime_parity"],
                "minimum_requests_per_lane": 12, "minimum_independent_runs": 1, "minimum_measurement_seconds_per_lane": 0},
        "selection_policy": {"id": "laptop-rehearsal-throughput", "version": 1, "objective": "maximum_slo_qualified_output_throughput",
                             "lane_aggregation": "arithmetic_mean", "evidence_level": "screening", "qualification_before_scoring": True, "cost_tie_breaker": False},
        "candidates": [candidate(control_a, workload_digest, True), candidate(trial, workload_digest, trial["outputs"] == control_a["outputs"])],
    }
    evidence_path = work / "optimization-evidence.json"
    evidence_path.write_text(json.dumps(evidence, indent=2) + "\n")

    infercrane = work / "infercrane"
    subprocess.run(["go", "build", "-o", str(infercrane), "./cmd/infercrane"], cwd=ROOT, check=True)
    evaluation = json.loads(subprocess.run([str(infercrane), "optimize", "evidence", "evaluate", "--file", str(evidence_path), "--output", "json"], check=True, capture_output=True, text=True).stdout)
    (work / "evaluation.json").write_text(json.dumps(evaluation, indent=2) + "\n")

    print("== result")
    print(f"   control drift between the two control runs: {drift:.1%}" + ("  (measurements unstable; rerun on a quiet machine)" if drift > 0.10 else ""))
    print(f"   output parity with control: {'match' if trial['outputs'] == control_a['outputs'] else 'DIFFERENT'}")
    for row in evaluation.get("candidates", []):
        score = row.get("score")
        status = "qualified" if row.get("qualified") else "rejected: " + "; ".join(row.get("rejection_reasons") or [])
        print(f"   {row['candidate_id']}: {status}" + (f", score {score:.1f}" if score is not None else ""))
    print(f"   winner: {evaluation.get('winner_id') or 'none'}")
    print(f"   evidence: {evidence_path}")
    print("   These are laptop measurements. They prove the decision path, not datacenter GPU performance.")
    return 0


if __name__ == "__main__":
    sys.exit(main())
