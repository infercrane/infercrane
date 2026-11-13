#!/usr/bin/env python3
"""Live view for recording the local optimization PoC.

Serves http://127.0.0.1:3300: one page with live requests and throughput per
serving pod, where time goes (reading prompts, generating tokens, queueing),
KV cache use, hardware use of the cluster node, the campaign's progress, and
the measured results of each setup against the current one.

Data comes only from real sources: each pod's /metrics through the
Kubernetes API proxy, `docker stats` of the kind node, and the InferCrane
control API. It reads only. On a laptop the hardware panel shows the CPU node,
never invented GPU numbers.
"""

from __future__ import annotations

import http.server
import importlib.util
import json
import subprocess
import sys
import threading
import time
import urllib.request
from pathlib import Path

HERE = Path(__file__).resolve().parent
ROOT = HERE.parents[1]
STATE_FILE = ROOT / ".infercrane" / "poc" / "state.json"
CONTEXT = "kind-infercrane-poc"
NAMESPACE = "infercrane-system"
NODE = "infercrane-poc-control-plane"
API = "http://127.0.0.1:18080"
PORT = 3300
HISTORY = 90  # samples kept per series (about 3 minutes at 2 s)

spec = importlib.util.spec_from_file_location("pocmonitor", ROOT / "tools" / "poc-monitor" / "monitor.py")
monitor = importlib.util.module_from_spec(spec)
sys.modules["pocmonitor"] = monitor
spec.loader.exec_module(monitor)

LOCK = threading.Lock()
VIEW: dict = {"pods": {}, "node": {}, "campaign": None, "results": [], "labels": {}, "updated": 0, "notes": []}
ENDPOINT = __import__("re").compile(r"http://(infercrane-[0-9a-f]+)\.")
SERVER_SECONDS = {
    "prefill": ("llamacpp:prompt_seconds_total", "vllm:request_prefill_time_seconds_sum"),
    "decode": ("llamacpp:tokens_predicted_seconds_total", "vllm:request_decode_time_seconds_sum"),
    "queue": ("vllm:request_queue_time_seconds_sum",),
}


def api(path: str) -> dict:
    key = json.loads(STATE_FILE.read_text())["api_key"]
    request = urllib.request.Request(API + path, headers={"Authorization": f"Bearer {key}"})
    with urllib.request.urlopen(request, timeout=10) as response:
        return json.loads(response.read())


def kubectl(*args: str, timeout: float = 8) -> str:
    completed = subprocess.run(["kubectl", "--context", CONTEXT, *args], capture_output=True, text=True, timeout=timeout)
    if completed.returncode != 0:
        raise RuntimeError(completed.stderr.strip()[:200])
    return completed.stdout


def collect_pods(previous: dict) -> dict:
    pods = json.loads(kubectl("get", "pods", "-n", NAMESPACE, "-l", "app.kubernetes.io/managed-by=infercrane", "-o", "json"))["items"]
    now = time.time()
    current: dict = {}
    for pod in pods:
        name = pod["metadata"]["name"]
        entry = previous.get(name, {"series": [], "last": {}, "t": 0})
        entry["phase"] = pod.get("status", {}).get("phase", "")
        if entry["phase"] == "Running":
            try:
                values = monitor.parse_metrics(kubectl("get", "--raw", f"/api/v1/namespaces/{NAMESPACE}/pods/{name}:8080/proxy/metrics", timeout=5))
            except Exception:  # noqa: BLE001 - starting server
                values = {}
            elapsed = now - entry["t"] if entry["t"] else 0
            sample = {"t": now}
            for key, names in {**monitor.COUNTERS, **SERVER_SECONDS}.items():
                value = monitor.pick(values, names)
                last = entry["last"].get(key)
                sample[key] = (value - last) / elapsed if value is not None and last is not None and elapsed > 0 and value >= last else None
                if value is not None:
                    entry["last"][key] = value
            for key, names in monitor.GAUGES.items():
                sample[key] = monitor.pick(values, names)
            sample["engine"] = monitor.engine_name(values) if values else "starting"
            entry["t"] = now
            entry["series"] = (entry["series"] + [sample])[-HISTORY:]
        current[name] = entry
    return current


def collect_node() -> dict:
    completed = subprocess.run(["docker", "stats", "--no-stream", "--format", "{{json .}}", NODE], capture_output=True, text=True, timeout=10)
    if completed.returncode != 0:
        return {}
    stats = json.loads(completed.stdout.strip().splitlines()[0])
    cpus = int(subprocess.run(["docker", "info", "--format", "{{.NCPU}}"], capture_output=True, text=True).stdout.strip() or 0)
    return {"cpu_percent": float(stats.get("CPUPerc", "0%").rstrip("%") or 0), "cpus": cpus, "memory": stats.get("MemUsage", ""), "memory_percent": float(stats.get("MemPerc", "0%").rstrip("%") or 0), "kind": "CPU node (laptop, no GPU)"}


def service_labels(names: dict) -> dict:
    labels = {}
    for deployment, label in names.items():
        try:
            detail = json.dumps(api(f"/api/v1/deployments/{deployment}"))
        except Exception:  # noqa: BLE001 - not created yet or cleaned up
            continue
        for service in set(ENDPOINT.findall(detail)):
            labels[service] = label
    return labels


def collect_campaign() -> tuple[dict | None, list, dict]:
    state = json.loads(STATE_FILE.read_text())
    campaign_id = state.get("campaign")
    if not campaign_id:
        return None, [], service_labels({"poc-baseline": "live endpoint (current setup)"})
    campaign = api(f"/api/v1/optimization/campaigns/{campaign_id}")
    record = campaign.get("campaign", campaign)
    labels = state.get("labels") or {}
    results = []
    for candidate in record.get("candidates", []):
        candidate["label"] = labels.get(candidate.get("proposal_candidate_id", ""), candidate.get("proposal_candidate_id", "")[:8])
        name = candidate.get("deployment_name")
        if not name or not candidate.get("benchmark_id"):
            continue
        try:
            rows = api(f"/api/v1/deployments/{name}/benchmarks")
        except Exception:  # noqa: BLE001 - deployment cleaned up
            continue
        rows = rows.get("data", rows.get("benchmarks", [])) if isinstance(rows, dict) else rows
        row = next((r for r in rows if r.get("id") == candidate["benchmark_id"]), None)
        if row:
            workload = row.get("workload", row.get("workload_json"))
            workload = json.loads(workload) if isinstance(workload, str) and workload else (workload or {})
            results.append({"label": candidate["label"], "state": candidate.get("state"), "output_tps": row.get("output_token_throughput"), "ttft_p95_ms": row.get("ttft_p95_ms"), "latency_p95_ms": row.get("latency_p95_ms"), "requests": row.get("request_count"), "failed": row.get("failed"), "measured": workload.get("measured"), "failure_code": candidate.get("failure_code")})
    names = {"poc-baseline": "live endpoint (current setup)"} | {c["deployment_name"]: c["label"] for c in record.get("candidates", []) if c.get("deployment_name")}
    return record, results, service_labels(names)


def loop() -> None:
    last_node = last_campaign = 0.0
    while True:
        notes = []
        try:
            pods = collect_pods(VIEW["pods"])
        except Exception as error:  # noqa: BLE001 - shown on the page
            pods, notes = VIEW["pods"], [f"pods: {error}"]
        node, campaign, results, labels = VIEW["node"], VIEW["campaign"], VIEW["results"], VIEW["labels"]
        if time.time() - last_node > 4:
            try:
                node = collect_node()
            except Exception as error:  # noqa: BLE001
                notes.append(f"node: {error}")
            last_node = time.time()
        if time.time() - last_campaign > 5:
            try:
                campaign, results, labels = collect_campaign()
            except Exception as error:  # noqa: BLE001
                notes.append(f"campaign: {error}")
            last_campaign = time.time()
        with LOCK:
            VIEW.update(pods=pods, node=node, campaign=campaign, results=results, labels=labels, updated=time.time(), notes=notes)
        time.sleep(2)


NOMINAL_HOURLY_USD = 0.25  # the harness's stated laptop rate card


def cost_per_million(tps: float | None) -> float | None:
    return NOMINAL_HOURLY_USD / (tps * 3600) * 1_000_000 if tps else None


def progress() -> dict:
    """Every measurement run as a point, and the running best across setups."""
    state = json.loads(STATE_FILE.read_text())
    campaign_id = state.get("campaign")
    if not campaign_id:
        return {"points": [], "setups": [], "plan": state.get("plan") or []}
    record = api(f"/api/v1/optimization/campaigns/{campaign_id}")
    record = record.get("campaign", record)
    labels = state.get("labels") or {}
    points, setups = [], []
    best = None
    for candidate in sorted(record.get("candidates", []), key=lambda c: c.get("rank", 0)):
        label = labels.get(candidate.get("proposal_candidate_id", ""), candidate.get("id", "")[:8])
        setup = {"label": label, "state": candidate.get("state"), "failure_code": candidate.get("failure_code"), "decision": None, "mean_cost": None, "kept": False}
        if candidate.get("benchmark_id") and candidate.get("deployment_name"):
            try:
                listing = api(f"/api/v1/deployments/{candidate['deployment_name']}/benchmarks")
            except Exception:  # noqa: BLE001 - cleaned up
                listing = {}
            rows = listing.get("data", []) if isinstance(listing, dict) else listing
            row = next((r for r in rows if r.get("id") == candidate["benchmark_id"]), None)
            if row:
                workload = row.get("workload")
                workload = json.loads(workload) if isinstance(workload, str) and workload else (workload or {})
                measured = workload.get("measured") or {}
                own = []
                for run in measured.get("runs") or []:
                    cost = cost_per_million(run.get("output_token_throughput"))
                    if cost is None:
                        continue
                    arm = "candidate" if run.get("arm", "").startswith("candidate") or run.get("arm") == "candidate" else run.get("arm", "")
                    points.append({"x": len(points) + 1, "y": cost, "arm": arm, "setup": label})
                    if arm == "candidate" or not setups:
                        own.append(cost)
                if not own:
                    own = [c for c in [cost_per_million(row.get("output_token_throughput"))] if c is not None]
                setup["mean_cost"] = sum(own) / len(own) if own else None
                comparison = (measured.get("comparison") or {}).get("output_token_throughput") or {}
                setup["decision"] = comparison.get("decision") or ("current" if not setups else None)
        if not setups and setup["mean_cost"] is not None:
            best = setup["mean_cost"]
            setup["kept"], setup["decision"] = True, "current"
        elif setup["decision"] == "improved" and setup["mean_cost"] is not None and (best is None or setup["mean_cost"] < best):
            best, setup["kept"] = setup["mean_cost"], True
        setup["best_after"] = best
        setups.append(setup)
    return {"points": points, "setups": setups, "campaign_state": record.get("state"), "plan": state.get("plan") or []}


def journey() -> dict:
    """Steps, recommendation cards and verdicts for the recommendation journey page."""
    state = json.loads(STATE_FILE.read_text())
    data = progress() if state.get("campaign") else {"setups": [], "points": [], "campaign_state": None}
    with LOCK:
        pods = {name: (pod.get("series") or [{}])[-1] for name, pod in VIEW.get("pods", {}).items()}
    live_server = {}
    for sample in pods.values():
        prefill, decode = sample.get("prefill") or 0, sample.get("decode") or 0
        if prefill + decode > live_server.get("busy", 0):
            live_server = {"busy": prefill + decode, "prefill_share": prefill / (prefill + decode) if prefill + decode else None,
                           "waiting": sample.get("waiting"), "running": sample.get("running"), "output_tps": sample.get("output_tokens"), "prompt_tps": sample.get("prompt_tokens")}
    if not live_server and pods:
        sample = next(iter(pods.values()))
        live_server = {"busy": 0, "prefill_share": None, "waiting": sample.get("waiting"), "running": sample.get("running"), "output_tps": sample.get("output_tokens"), "prompt_tps": sample.get("prompt_tokens")}
    return {"diagnosis": state.get("diagnosis"), "plan": state.get("plan") or [], "campaign": state.get("campaign"), "campaign_state": data.get("campaign_state"),
            "setups": data.get("setups", []), "quality": state.get("quality") or {}, "labels": state.get("labels") or {}, "result": state.get("result"), "live": live_server,
            "generated_sha": state.get("generated_model_sha256")}


class Handler(http.server.BaseHTTPRequestHandler):
    def do_GET(self) -> None:  # noqa: N802 - stdlib name
        if self.path.startswith("/state"):
            with LOCK:
                body = json.dumps(VIEW, default=str).encode()
            content_type = "application/json"
        elif self.path.startswith("/journey-data"):
            try:
                body = json.dumps(journey(), default=str).encode()
            except Exception as error:  # noqa: BLE001 - shown on the page
                body = json.dumps({"error": str(error)}).encode()
            content_type = "application/json"
        elif self.path.startswith("/journey"):
            body = (HERE / "journey.html").read_bytes()
            content_type = "text/html; charset=utf-8"
        elif self.path.startswith("/progress-data"):
            try:
                body = json.dumps(progress(), default=str).encode()
            except Exception as error:  # noqa: BLE001 - shown on the page
                body = json.dumps({"error": str(error), "points": [], "setups": []}).encode()
            content_type = "application/json"
        elif self.path.startswith("/progress"):
            body = (HERE / "progress.html").read_bytes()
            content_type = "text/html; charset=utf-8"
        elif self.path in ("/", "/index.html"):
            body = (HERE / "live.html").read_bytes()
            content_type = "text/html; charset=utf-8"
        else:
            self.send_error(404)
            return
        self.send_response(200)
        self.send_header("Content-Type", content_type)
        self.send_header("Cache-Control", "no-store")
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, *_: object) -> None:
        return


def main() -> int:
    threading.Thread(target=loop, daemon=True).start()
    server = http.server.ThreadingHTTPServer(("127.0.0.1", PORT), Handler)
    print(f"live view on http://127.0.0.1:{PORT}", flush=True)
    server.serve_forever()
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
