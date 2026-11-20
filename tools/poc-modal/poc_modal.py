#!/usr/bin/env python3
"""One optimization campaign through the real InferCrane control plane on Modal H100s.

    poc_modal.py up        kind cluster, local registry, model server proxy image, Postgres and
                           `infercrane serve` (Kubernetes provider) in the cluster. No GPU.
    poc_modal.py download  stage the pinned Qwen3-8B weights on a Modal volume (CPU only)
    poc_modal.py dry       one lease through the broker, tunnel and proxy on a Modal CPU
                           container with a stand-in server (no GPU)
    poc_modal.py run       baseline on an H100, Mooncake replay through the gateway, diagnosis,
                           Replay capture, a campaign (current setup plus two candidates
                           measured in turns), the built-in answer check and ranking. Stops at
                           the proposal: nothing is activated.
    poc_modal.py down      delete the cluster and registry

Development only. Modal is not an InferCrane provider: the control plane runs its
Kubernetes provider unchanged, and each model server pod is a proxy
(internal/testtools/modal-gpu-proxy) to one H100 container that a laptop broker
(broker.py) starts with the pod's exact vLLM arguments. Built on tools/poc-local/poc.py.
"""

from __future__ import annotations

import argparse
import concurrent.futures
import datetime as dt
import hashlib
import json
import os
import shutil
import signal
import subprocess
import sys
import time
import urllib.error
import urllib.request
from pathlib import Path

HERE = Path(__file__).resolve().parent
sys.path.insert(0, str(HERE.parent / "poc-local"))
import poc  # noqa: E402

ROOT = poc.ROOT
MODAL_CLIENT = "modal==1.6.4"
# Pinned in modal_gpu.py as well; the broker refuses any other model.
MODEL_ID = "Qwen/Qwen3-8B"
MODEL_REVISION = "b968826d9c46dd6066d109eabc6255188de91218"
VLLM_IMAGE = "vllm/vllm-openai@sha256:953d3a06d5e64ab582985cd7401289d3abf2a2c14ef2158e9a84313daeec77d7"
ENGINE_VERSION = "vllm-openai-0.22.1-953d3a06"
# Modal list price for one H100 SXM5, $0.001097 per second (modal.com/pricing, read
# 2026-11-20). GPU only: the container's CPU and memory are billed on top and are in the
# broker's estimate and Modal's billing report, not in this rate.
H100_HOURLY_USD = round(0.001097 * 3600, 4)
APPROVAL_HOURS = 2
BROKER_PORT = 18095
BROKER_HOST = "host.docker.internal"  # the Mac host as seen from kind pods (Docker Desktop)
PROFILE, OBJECTIVE = "throughput", "throughput"  # 32 concurrent streams per measured run
MAX_INPUT_TOKENS = 30000  # Qwen3-8B serves 40,960 positions; leave room for the answer

# Same names as the laptop PoC but separate resources, so both can exist at once.
poc.STATE_DIR = ROOT / ".infercrane" / "poc-modal"
poc.STATE_FILE = poc.STATE_DIR / "state.json"
poc.OUTPUT = poc.STATE_DIR / "output"
poc.CLUSTER = "infercrane-gpu-poc"
poc.CONTEXT = f"kind-{poc.CLUSTER}"
poc.NODE = f"{poc.CLUSTER}-control-plane"
poc.REGISTRY = "infercrane-gpu-poc-registry"
poc.REGISTRY_PORT = 5002
poc.CONTROL_PLANE_IMAGE = "infercrane-gpu-poc/control-plane:dev"
poc.ACCELERATOR_RESOURCE = "infercrane.dev/modal-h100-lease"
poc.ACCELERATOR = "H100"
poc.NOMINAL_HOURLY_USD = H100_HOURLY_USD
poc.APPROVAL_HOURS = APPROVAL_HOURS
poc.API_PORT = 18081
poc.BASELINE = "gpu-baseline"
poc.OPTIMIZED = "gpu-optimized"
poc.MODEL_ID = MODEL_ID
poc.MODEL_REVISION = MODEL_REVISION
poc.MOONCAKE_BLOCK_TOKENS = 512
STATE_DIR, OUTPUT, log, fail = poc.STATE_DIR, poc.OUTPUT, poc.log, poc.fail
LEDGER = STATE_DIR / "broker-ledger.json"

CONTROL = ("Your current setup (vLLM defaults)", "vllm-defaults", [])
# What the harness may try on vLLM on an H100, chosen from the measured bottleneck.
OPTIONS = {
    "fp8_weights": ("FP8 weights (quantized by vLLM at load)", "vllm-fp8-weights", ["--quantization", "fp8"],
                    "an 8B model on an H100 is limited by weight reads in decode and by matrix math in prefill; FP8 halves both (answers re-checked)"),
    "fp8_kv": ("Compact FP8 KV cache", "vllm-fp8-kv-cache", ["--kv-cache-dtype", "fp8"],
               "the KV cache filled up or requests were preempted, so store twice as many tokens"),
    # The planner's own vLLM throughput setting for this model on an H100.
    "prefill_batch": ("Bigger prefill batches", "vllm-batched-tokens-16k", ["--max-num-batched-tokens", "16384"],
                      "most time went to reading long prompts while requests waited, so prefill more tokens per step"),
}
CANDIDATES = [CONTROL]


def sh(*command: str, **kwargs) -> str:
    return poc.run(*command, **kwargs)


def state() -> dict:
    return poc.state()


def save(values: dict) -> None:
    poc.save(values)


# ---------------------------------------------------------------- up


def proxy_image() -> str:
    tag = f"localhost:{poc.REGISTRY_PORT}/infercrane-gpu-poc/modal-gpu-proxy:dev"
    log("setup", "building the model server proxy image (internal/testtools/modal-gpu-proxy)")
    sh("docker", "build", "-q", "-f", str(poc.POC / "modal-gpu-proxy.Dockerfile"), "-t", tag, str(ROOT), quiet=True)
    sh("docker", "push", "-q", tag, quiet=True)
    pushed = sh("docker", "inspect", "--format", "{{index .RepoDigests 0}}", tag, capture=True).strip()
    if "@sha256:" not in pushed:
        fail(f"could not read a registry digest for {tag}")
    return f"{poc.REGISTRY_ALIAS}/infercrane-gpu-poc/modal-gpu-proxy@{pushed.split('@', 1)[1]}"


def advertise_leases(count: int) -> None:
    """The node advertises one schedulable slot per remote H100 the broker may run, labeled
    H100 because the work runs on an H100; the resource name says it is a Modal lease."""
    poc.kubectl("label", "node", poc.NODE, f"{poc.ACCELERATOR_LABEL}={poc.ACCELERATOR}", "--overwrite")
    escaped = poc.ACCELERATOR_RESOURCE.replace("~", "~0").replace("/", "~1")
    patch = json.dumps([{"op": "add", "path": f"/status/capacity/{escaped}", "value": str(count)},
                        {"op": "add", "path": f"/status/allocatable/{escaped}", "value": str(count)}])
    sh("kubectl", "--context", poc.CONTEXT, "patch", "node", poc.NODE, "--subresource=status", "--type=json", "-p", patch, quiet=True)
    for _ in range(30):
        if "modal-h100-lease" in poc.kubectl("get", "node", poc.NODE, "-o", "jsonpath={.status.allocatable}"):
            break
        time.sleep(1)
    else:
        fail("the node did not report the Modal lease resource as allocatable")
    log("setup", f"node advertises {count} x {poc.ACCELERATOR_RESOURCE} (one per remote H100 the broker may run)")


def rate_card() -> str:
    now = dt.datetime.now(dt.timezone.utc).replace(microsecond=0)
    stamp = lambda moment: moment.isoformat().replace("+00:00", "Z")  # noqa: E731
    row = {"cloud": "kubernetes", "region": "", "gpu": poc.ACCELERATOR, "gpu_count": 1, "replicas": 1, "hourly_usd": H100_HOURLY_USD, "currency": "USD",
           "source": "operator-rate-card/modal-h100-sxm5-list-2026-11-20", "authority": "account_contract", "cost_scope": "instance_total",
           "observed_at": stamp(now - dt.timedelta(minutes=5)), "valid_until": stamp(now + dt.timedelta(days=7))}
    return json.dumps([row], separators=(",", ":"))


def up() -> None:
    for tool in ("docker", "kubectl", "go", "curl", "uv"):
        if not shutil.which(tool):
            fail(f"{tool} is required")
    if not state().get("api_key"):
        save({"api_key": __import__("secrets").token_hex(24)})
    kind = poc.kind_binary()
    poc.ensure_registry()
    poc.ensure_cluster(kind)
    advertise_leases(3)
    image = proxy_image()
    save({"model_image": image})
    log("setup", "building the control plane image")
    sh("docker", "build", "-q", "--target", "runtime", "-t", poc.CONTROL_PLANE_IMAGE, str(ROOT), quiet=True)
    sh(kind, "load", "docker-image", poc.CONTROL_PLANE_IMAGE, "--name", poc.CLUSTER, quiet=True)
    for manifest in ("namespace.yaml", "service-accounts.yaml", "provider-rbac.yaml"):
        poc.kubectl("apply", "-f", str(ROOT / "deploy" / "kubernetes" / "base" / manifest))
    secret = poc.kubectl("-n", poc.NAMESPACE, "create", "secret", "generic", "infercrane-worker", f"--from-literal=api-key={state()['api_key']}", "--dry-run=client", "-o", "json")
    subprocess.run(["kubectl", "--context", poc.CONTEXT, "apply", "-f", "-"], input=secret, text=True, check=True, capture_output=True)
    rendered = (poc.POC / "manifests.yaml").read_text()
    for key, value in {"POSTGRES_IMAGE": poc.POSTGRES_IMAGE, "CONTROL_PLANE_IMAGE": poc.CONTROL_PLANE_IMAGE, "MODEL_IMAGE": image,
                       "ACCELERATOR_RESOURCE": poc.ACCELERATOR_RESOURCE, "ACCELERATOR_LABEL": poc.ACCELERATOR_LABEL, "PRICES_JSON": rate_card()}.items():
        rendered = rendered.replace(f"@@{key}@@", value)
    subprocess.run(["kubectl", "--context", poc.CONTEXT, "apply", "-f", "-"], input=rendered, text=True, check=True, capture_output=True)
    log("setup", "starting Postgres and the InferCrane control plane in the cluster")
    poc.kubectl("-n", poc.NAMESPACE, "rollout", "status", "deploy/postgres", "--timeout=300s")
    poc.kubectl("-n", poc.NAMESPACE, "rollout", "restart", "deploy/infercrane")
    poc.kubectl("-n", poc.NAMESPACE, "rollout", "status", "deploy/infercrane", "--timeout=300s")
    poc.port_forward()
    poc.cli("doctor", "--kubernetes", check=False)
    log("ready", f"control plane at http://127.0.0.1:{poc.API_PORT}; no GPU is running")


# ---------------------------------------------------------------- broker


def broker_command(budget: float, dry: bool, max_starts: int) -> list[str]:
    command = ["uv", "run", "--quiet", "--no-project", "--with", MODAL_CLIENT, "python", str(HERE / "broker.py"), "--token-file", str(poc.STATE_FILE),
               "--budget-usd", str(budget), "--listen", f"127.0.0.1:{BROKER_PORT}", "--ledger", str(LEDGER), "--max-starts", str(max_starts)]
    return command + (["--dry"] if dry else [])


class BrokerProcess:
    """The broker runs for exactly as long as the run; stopping it stops every container."""

    def __init__(self, budget: float, dry: bool, max_starts: int) -> None:
        self.log_path = STATE_DIR / ("broker-dry.log" if dry else "broker.log")
        self.process = subprocess.Popen(broker_command(budget, dry, max_starts), stdout=self.log_path.open("a"), stderr=subprocess.STDOUT, cwd=ROOT, start_new_session=True)
        save({"broker_pid": self.process.pid})
        for _ in range(120):
            if self.process.poll() is not None:
                fail(f"the broker exited: {self.log_path.read_text()[-1500:]}")
            try:
                self.request("GET", "/v1/ledger")
                return
            except (urllib.error.URLError, ConnectionError):
                time.sleep(1)
        fail("the broker did not start")

    def request(self, method: str, path: str, body: dict | None = None) -> dict:
        data = json.dumps(body).encode() if body is not None else None
        request = urllib.request.Request(f"http://127.0.0.1:{BROKER_PORT}{path}", method=method, data=data)
        request.add_header("Authorization", f"Bearer {state()['api_key']}")
        request.add_header("Content-Type", "application/json")
        with urllib.request.urlopen(request, timeout=60) as response:
            raw = response.read()
        return json.loads(raw) if raw else {}

    def ledger(self) -> dict:
        return self.request("GET", "/v1/ledger")

    def stop(self) -> dict:
        ledger: dict = {}
        if self.process.poll() is None:
            try:
                ledger = self.request("POST", "/v1/stop-all")
            except Exception:  # noqa: BLE001 - stopping the process stops the app anyway
                pass
            time.sleep(10)
            self.process.send_signal(signal.SIGTERM)
            try:
                self.process.wait(timeout=90)
            except subprocess.TimeoutExpired:
                self.process.kill()
        save({"broker_pid": None})
        return json.loads(LEDGER.read_text()) if LEDGER.exists() else ledger


def check_broker_reachable() -> None:
    """The pods reach the broker on the Mac host; prove it from inside the cluster first."""
    probe = ("import urllib.request, urllib.error\n"
             f"try:\n    urllib.request.urlopen('http://{BROKER_HOST}:{BROKER_PORT}/v1/ledger', timeout=5)\n"
             "except urllib.error.HTTPError as e:\n    print(e.code)\n")
    out = poc.kubectl("-n", poc.NAMESPACE, "exec", "deploy/infercrane", "--", "python3", "-c", probe, check=False).strip()
    if out != "401":
        fail(f"pods cannot reach the broker at http://{BROKER_HOST}:{BROKER_PORT} (got {out!r})")
    log("broker", f"reachable from the cluster at http://{BROKER_HOST}:{BROKER_PORT}")


# ---------------------------------------------------------------- run


def workload() -> dict:
    # The base vLLM arguments live in the workload command, so the control plane's spec
    # records every argument the remote server runs with; candidates add runtime args.
    command = ["/modal-gpu-proxy", "--broker", f"http://{BROKER_HOST}:{BROKER_PORT}", "--port", "${PORT}", "--model", "${MODEL}", "--", "--served-model-name", "${MODEL}"]
    return {"image": state()["model_image"], "command": command, "protocol": "openai", "port": poc.SERVER_PORT, "readiness_path": "/health", "models_path": "/v1/models",
            "metrics_path": "/metrics", "cancellation": "http-disconnect", "drain": "connection", "shutdown_grace_seconds": 20}


def runtime(args: list[str]) -> dict:
    return {"engine": "custom-oci", "version": ENGINE_VERSION, "args": args, "workload": workload()}


def deploy_baseline() -> None:
    spec = {"apiVersion": "infercrane.dev/v1", "kind": "Deployment", "name": poc.BASELINE, "model": {"id": MODEL_ID, "revision": MODEL_REVISION}, "runtime": runtime([]),
            "compute": {"mode": "elastic"}, "resources": {"gpu": poc.ACCELERATOR, "gpu_count": 1}, "provider": {"cloud": "kubernetes", "adapter": "kubernetes", "region": ""},
            "scaling": {"min_replicas": 1, "max_replicas": 1}, "routing": {"strategy": "round-robin"}}
    path = STATE_DIR / "baseline.yaml"
    path.write_text(json.dumps(spec, indent=2))
    digest = hashlib.sha256(json.dumps(spec, sort_keys=True).encode()).hexdigest()[:12]
    log("deploy", f"current setup: {MODEL_ID} on vLLM 0.22.1 defaults, one H100 ({poc.BASELINE}); first start loads the image and weights")
    poc.cli("deploy", str(path), "--idempotency-key", f"{poc.BASELINE}-{digest}", "--wait", capture=False)


def replay_mooncake(requests_total: int, input_scale: int, max_output: int, workers: int) -> dict:
    """Replays the first rows of the Mooncake trace through the gateway at their recorded
    arrival times. Inputs are scaled 1/input_scale and capped to fit the model."""
    rows = poc.mooncake_rows()[:requests_total]
    start = rows[0]["timestamp"]
    block = max(1, poc.MOONCAKE_BLOCK_TOKENS // input_scale)
    log("traffic", f"replaying {len(rows)} requests from the Mooncake (Kimi) production trace at their recorded arrival times: shared prefixes kept, inputs scaled 1/{input_scale}, capped at {MAX_INPUT_TOKENS} tokens")
    key, began, failures = state()["api_key"], time.monotonic(), []
    sent = {"input_words": 0, "max_tokens": 0}

    def one(row: dict) -> int:
        delay = (row["timestamp"] - start) / 1000 - (time.monotonic() - began)
        if delay > 0:
            time.sleep(delay)
        target = min(MAX_INPUT_TOKENS, max(8, row["input_length"] // input_scale))
        words: list[str] = []
        for hash_id in row.get("hash_ids") or [row["timestamp"]]:
            words += poc.block_text(hash_id, block)
        words = (words + poc.block_text(row["timestamp"], target))[:target]
        shared, unique = words[:-block] or words[:1], words[-block:]
        output = max(1, min(max_output, row["output_length"]))
        sent["input_words"] += len(words)
        sent["max_tokens"] += output
        body = {"model": poc.BASELINE, "stream": False, "temperature": 0, "max_tokens": output, "ignore_eos": True,
                "messages": [{"role": "system", "content": " ".join(shared)}, {"role": "user", "content": " ".join(unique)}]}
        request = urllib.request.Request(f"http://127.0.0.1:{poc.API_PORT}/v1/chat/completions", data=json.dumps(body).encode(), method="POST")
        session = (row.get("hash_ids") or [0])[0]
        for header, value in {"Authorization": f"Bearer {key}", "Content-Type": "application/json", "X-InferCrane-Session-ID": f"mooncake-{session}", "X-InferCrane-Parent-Session-ID": "mooncake-replay"}.items():
            request.add_header(header, value)
        try:
            with urllib.request.urlopen(request, timeout=600) as response:
                payload = json.loads(response.read())
        except (urllib.error.URLError, OSError, json.JSONDecodeError) as error:
            failures.append(str(getattr(error, "code", "")) or type(error).__name__)
            return 0
        return int((payload.get("usage") or {}).get("completion_tokens", 0))

    with concurrent.futures.ThreadPoolExecutor(max_workers=workers) as pool:
        tokens = sum(pool.map(one, rows))
    seconds = time.monotonic() - began
    note = f" · {len(failures)} of {len(rows)} requests failed ({', '.join(sorted(set(failures)))})" if failures else ""
    log("traffic", f"done in {seconds:.0f} s: {tokens} output tokens reported by the server{note} · trace: kvcache-ai/Mooncake FAST'25 (Apache-2.0)")
    if len(failures) > len(rows) // 2:
        fail("most replayed requests failed; check the gateway and the model server before measuring")
    return {"requests": len(rows), "failed": len(failures), "seconds": round(seconds, 1), "output_tokens": tokens, "input_words_sent": sent["input_words"],
            "input_scale": input_scale, "max_input_tokens": MAX_INPUT_TOKENS, "max_output_tokens": max_output}


def pod_get(pod: str, path: str) -> str:
    """Reads a pod's endpoint through the Kubernetes API; raises OSError when it cannot."""
    completed = subprocess.run(["kubectl", "--context", poc.CONTEXT, "get", "--raw", f"/api/v1/namespaces/{poc.NAMESPACE}/pods/{pod}:{poc.SERVER_PORT}/proxy{path}"],
                               capture_output=True, text=True, timeout=30)
    if completed.returncode != 0:
        raise OSError(f"{pod}{path}: {completed.stderr.strip()[-300:]}")
    return completed.stdout


def vllm_metrics(pod: str) -> dict[str, float]:
    values: dict[str, float] = {}
    for line in pod_get(pod, "/metrics").splitlines():
        if line and not line.startswith("#"):
            parts = line.split()
            name = parts[0].split("{")[0]
            try:
                values[name] = values.get(name, 0.0) + float(parts[-1])
            except (ValueError, IndexError):
                continue
    return values


class Observer:
    """Samples the current setup's vLLM metrics (through the proxy) while the traffic runs."""

    GAUGES = ("vllm:num_requests_waiting", "vllm:kv_cache_usage_perc", "vllm:num_requests_running")

    def __init__(self) -> None:
        self.pod = poc.baseline_pod()
        self.start = vllm_metrics(self.pod)
        self.samples: dict[str, list[float]] = {name: [] for name in self.GAUGES}
        self.stop = False
        self.thread = __import__("threading").Thread(target=self.loop, daemon=True)
        self.thread.start()

    def loop(self) -> None:
        while not self.stop:
            try:
                current = vllm_metrics(self.pod)
                for name in self.GAUGES:
                    if name in current:
                        self.samples[name].append(current[name])
            except Exception:  # noqa: BLE001 - one sample can fail while the server is busy
                pass
            time.sleep(1)

    def finish(self) -> dict:
        self.stop = True
        self.thread.join(timeout=5)
        for attempt in range(5):
            try:
                end = vllm_metrics(self.pod)
                break
            except (OSError, subprocess.TimeoutExpired):
                if attempt == 4:
                    raise
                time.sleep(5)
        delta = lambda key: max(0.0, end.get(key, 0.0) - self.start.get(key, 0.0))  # noqa: E731
        prefill, decode = delta("vllm:request_prefill_time_seconds_sum"), delta("vllm:request_decode_time_seconds_sum")
        total = prefill + decode or 1.0
        mean = lambda xs: sum(xs) / len(xs) if xs else 0.0  # noqa: E731
        queries, hits = delta("vllm:prefix_cache_queries_total"), delta("vllm:prefix_cache_hits_total")
        stats = {}
        try:
            stats = json.loads(pod_get(self.pod, "/proxy/stats"))
        except Exception:  # noqa: BLE001 - network evidence is optional
            pass
        return {"prefill_share": prefill / total, "decode_share": decode / total, "waiting_mean": mean(self.samples["vllm:num_requests_waiting"]),
                "waiting_peak": max(self.samples["vllm:num_requests_waiting"], default=0.0), "kv_usage_peak": max(self.samples["vllm:kv_cache_usage_perc"], default=0.0),
                "running_peak": max(self.samples["vllm:num_requests_running"], default=0.0), "preemptions": delta("vllm:num_preemptions_total"),
                "prefix_cache_hit_rate": hits / queries if queries else None, "prompt_tokens": delta("vllm:prompt_tokens_total"),
                "output_tokens": delta("vllm:generation_tokens_total"), "samples": len(self.samples["vllm:num_requests_waiting"]), "network": stats}


def plan(diagnosis: dict) -> list:
    hit = diagnosis["prefix_cache_hit_rate"]
    log("diagnose", f"while serving your traffic, the current setup spent {diagnosis['prefill_share']:.0%} of request time reading prompts and {diagnosis['decode_share']:.0%} writing answers; "
        f"{diagnosis['waiting_mean']:.1f} requests waited on average (peak {diagnosis['waiting_peak']:.0f}); KV cache peaked at {diagnosis['kv_usage_peak']:.0%}; "
        f"{diagnosis['preemptions']:.0f} preemptions; prefix cache hit rate {'unknown' if hit is None else f'{hit:.0%}'}")
    # Each pick carries the measured reason it was chosen; a pick without a measured
    # trigger says so instead of borrowing another option's reason.
    picks: list[tuple[str, str]] = [("fp8_weights", OPTIONS["fp8_weights"][3])]
    if diagnosis["kv_usage_peak"] >= 0.9 or diagnosis["preemptions"] > 0:
        picks.insert(0, ("fp8_kv", OPTIONS["fp8_kv"][3]))
    elif diagnosis["decode_share"] > diagnosis["prefill_share"]:
        picks.append(("fp8_kv", f"writing answers took {diagnosis['decode_share']:.0%} of request time, and with long shared contexts each decode step reads "
                                "as much KV cache as weights, so an FP8 KV cache halves those reads"))
    if diagnosis["prefill_share"] >= diagnosis["decode_share"] and diagnosis["waiting_mean"] >= 0.5:
        picks.append(("prefill_batch", OPTIONS["prefill_batch"][3]))
    for fallback in ("fp8_kv", "prefill_batch"):
        if fallback not in [key for key, _ in picks]:
            picks.append((fallback, "no measured trigger; tried as the next option on the list"))
    chosen = [CONTROL]
    for key, why in picks[:2]:
        label, profile, args, _ = OPTIONS[key]
        chosen.append((label, profile, args))
        log("plan", f"try “{label}”: {why}  [change: {' '.join(args)}]")
    CANDIDATES[:] = chosen
    save({"plan": [{"label": label, "profile": profile, "args": args} for label, profile, args in chosen], "diagnosis": diagnosis})
    return chosen


def proposal(digest: str) -> dict:
    # Kubernetes + H100 + vLLM has no live qualification row, so the planner only offers it
    # as a simulated path (as the laptop PoC's SIMULATED-CPU). The harness replaces the
    # runtime with the proxy workload anyway; the template supplies the campaign's shape.
    base = json.loads(poc.cli("optimize", "propose", MODEL_ID, "--model-revision", MODEL_REVISION, "--provider", "kubernetes", "--gpu", poc.ACCELERATOR, "--objective", OBJECTIVE,
                              "--profile", PROFILE, "--workload-fingerprint", f"sha256:{digest}", "--max-candidates", str(len(CANDIDATES)), "--source", "catalog",
                              "--include-simulated", "--output", "json"))
    template = next((c for c in base.get("candidates", []) if c["deployment"]["provider"]["adapter"] == "kubernetes"), None)
    if template is None:
        fail(f"the planner returned no Kubernetes template candidate: {json.dumps(base)[:600]}")
    candidates = []
    for rank, (label, profile, args) in enumerate(CANDIDATES, start=1):
        deployment = json.loads(json.dumps(template["deployment"]))
        deployment.pop("serving", None)
        deployment["name"] = poc.OPTIMIZED
        deployment["runtime"] = runtime(args)
        deployment["resources"] = dict(deployment.get("resources") or {}, gpu=poc.ACCELERATOR, gpu_count=1)
        # One H100 per setup: the comparison is per GPU, and the broker runs at most three.
        deployment["scaling"] = dict(deployment.get("scaling") or {}, min_replicas=1, max_replicas=1)
        identity = hashlib.sha256(json.dumps({"profile": profile, "deployment": deployment}, sort_keys=True).encode()).hexdigest()
        candidate = json.loads(json.dumps(template)) | {"id": identity, "rank": rank, "configuration_profile": profile, "deployment": deployment}
        candidate["features"] = [{"name": "runtime_args", "state": "candidate" if args else "baseline", "source": label}]
        candidates.append(candidate)
    base["candidates"] = candidates
    base["starting_point"] = dict(base["starting_point"], candidate_id=candidates[0]["id"])
    return base


def campaign_create(doc: dict) -> str:
    """The idempotency key covers the whole proposal and this run: the same planner input
    with other candidates is a different campaign, and a new run is a new campaign rather
    than a replay of an earlier, possibly cancelled one."""
    identity = json.dumps({"proposal": doc, "run": state()["run_id"]}, sort_keys=True)
    key = "poc-modal-" + hashlib.sha256(identity.encode()).hexdigest()[:24]
    created = poc.api("POST", "/api/v1/optimization/campaigns", {"proposal": doc, "intent": "new_endpoint"}, headers={"Idempotency-Key": key})
    campaign = created.get("id") or (created.get("campaign") or {}).get("id")
    if not campaign:
        fail(f"campaign create returned no id: {json.dumps(created)[:400]}")
    return campaign


def benchmark_row(candidate: dict) -> dict | None:
    try:
        rows = poc.api("GET", f"/api/v1/deployments/{candidate['deployment_name']}/benchmarks")
    except SystemExit:
        return None
    rows = rows.get("data", rows.get("benchmarks", [])) if isinstance(rows, dict) else rows
    row = next((r for r in rows if r.get("id") == candidate.get("benchmark_id")), None)
    if not row:
        return None
    workload_doc = row.get("workload", row.get("workload_json"))
    workload_doc = json.loads(workload_doc) if isinstance(workload_doc, str) and workload_doc else (workload_doc or {})
    keys = ("output_token_throughput", "request_throughput", "ttft_p50_ms", "ttft_p95_ms", "tpot_p95_ms", "latency_p50_ms", "latency_p95_ms", "request_count", "succeeded", "failed", "duration_seconds", "goodput")
    out = {key: row.get(key) for key in keys}
    out |= {"output_tps": row.get("output_token_throughput"), "comparison": ((workload_doc.get("measured") or {}).get("comparison") or {}),
            "confidence_95": workload_doc.get("confidence_95"), "concurrency": workload_doc.get("concurrency"), "request_count_per_run": workload_doc.get("request_count_per_run"),
            "profile_runs": workload_doc.get("profile_runs"), "workload_source": workload_doc.get("workload_source")}
    return out


poc.benchmark_row = benchmark_row


def network_stats() -> None:
    """Keeps each live pod's measured round trip to its remote GPU; pods are removed later."""
    pods = json.loads(poc.kubectl("-n", poc.NAMESPACE, "get", "pods", "-l", "app.kubernetes.io/managed-by=infercrane", "-o", "json"))["items"]
    known = state().get("network", {})
    for pod in pods:
        name = pod["metadata"]["name"]
        if pod.get("status", {}).get("phase") != "Running":
            continue
        try:
            known[name] = json.loads(pod_get(name, "/proxy/stats"))
        except Exception:  # noqa: BLE001 - optional evidence
            continue
    save({"network": known})


def wait_for_lease_end(broker: BrokerProcess, pod_prefix: str, timeout_s: int = 300) -> None:
    deadline = time.monotonic() + timeout_s
    while time.monotonic() < deadline:
        live = [lease for lease in broker.ledger()["leases"] if lease["state"] in ("requested", "starting", "ready", "stopping")]
        if not live:
            return
        time.sleep(5)
    fail(f"remote GPUs still running after the {pod_prefix} pods stopped; stop them with: poc_modal.py stop-gpus")


def run_campaign(budget: float, requests_total: int, reuse_capture: bool) -> None:
    if not state().get("model_image"):
        fail("run `poc_modal.py up` first")
    previous = state()
    if reuse_capture and not (previous.get("capture_digest") and previous.get("plan") and previous.get("diagnosis")):
        fail("--reuse-capture needs an earlier run's recorded traffic shape and diagnosis")
    poc.port_forward()
    poc.reset_previous()  # clears the plan and diagnosis; a reused capture keeps its own
    kept = {key: previous.get(key) for key in ("capture_digest", "plan", "diagnosis", "traffic")} if reuse_capture else {"traffic": None, "capture_digest": None}
    save({"network": {}, "run_started_at": time.time(), "run_id": __import__("secrets").token_hex(8)} | kept)
    broker = BrokerProcess(budget, dry=False, max_starts=6)
    log("broker", f"remote H100s capped at ${budget:.2f} for this run (estimate from Modal list prices), at most 3 at once")
    try:
        check_broker_reachable()
        if reuse_capture:
            digest, diagnosis = previous["capture_digest"], previous["diagnosis"]
            CANDIDATES[:] = [(row["label"], row["profile"], row["args"]) for row in previous["plan"]]
            log("capture", f"reusing the recorded traffic shape sha256:{digest[:12]}… and its diagnosis: {', '.join(row['label'] for row in previous['plan'][1:])}")
        else:
            deploy_baseline()
            observer = Observer()
            traffic = replay_mooncake(requests_total, input_scale=2, max_output=512, workers=96)
            save({"traffic": traffic})
            diagnosis = plan_from(observer)
            digest = poc.capture_replay()
            save({"capture_digest": digest})
            log("deploy", f"removing {poc.BASELINE}: its traffic shape is recorded, and its H100 is not needed for the campaign")
            poc.cli("delete", poc.BASELINE, "--yes", "--wait", check=False)
            wait_for_lease_end(broker, poc.BASELINE)
        doc = proposal(digest)
        labels = {candidate["id"]: CANDIDATES[index][0] for index, candidate in enumerate(doc["candidates"])}
        (STATE_DIR / "proposal.json").write_text(json.dumps(doc, indent=2))
        save({"labels": labels, "diagnosis": diagnosis})
        campaign = campaign_create(doc)
        save({"campaign": campaign})
        # The control plane splits the approved amount across setups and requires each share
        # to cover its rate for the whole approval window. It is the control plane's ceiling
        # at the stated rate, not the money spent; the broker cap above bounds that.
        approval = round(H100_HOURLY_USD * APPROVAL_HOURS * len(doc["candidates"]) * 1.05, 2)
        approved = lambda: poc.campaign_record(campaign).get("state") != "awaiting_approval"  # noqa: E731
        poc.cli("optimize", "approve", campaign, "--max-cost-usd", str(approval), "--expires-in", f"{APPROVAL_HOURS}h", repeatable=False, landed=approved)
        log("approve", f"campaign approved up to ${approval} at the stated H100 rate (${H100_HOURLY_USD}/h, {APPROVAL_HOURS} h window); real spend stays under the broker cap")
        record = watch(campaign, labels)
        finish(record, labels, broker)
    finally:
        ledger = broker.stop()
        (OUTPUT / "broker-ledger.json").parent.mkdir(parents=True, exist_ok=True)
        (OUTPUT / "broker-ledger.json").write_text(json.dumps(ledger, indent=2))
        log("broker", f"stopped; estimated remote spend ${ledger.get('estimated_usd', 0):.2f} (Modal billing is the authority: modal billing report --for today --show-resources)")


def plan_from(observer: Observer) -> dict:
    diagnosis = observer.finish()
    plan(diagnosis)
    return diagnosis


def watch(campaign: str, labels: dict) -> dict:
    """poc.watch, while a side thread keeps each live pod's network round trip."""
    import threading

    done = threading.Event()

    def sample() -> None:
        while not done.wait(30):
            try:
                network_stats()
            except Exception:  # noqa: BLE001 - optional evidence
                pass

    sampler = threading.Thread(target=sample, daemon=True)
    sampler.start()
    try:
        return poc.watch(campaign, labels, timeout_s=3 * 3600)
    finally:
        done.set()
        sampler.join(timeout=60)


# ---------------------------------------------------------------- report


def finish(record: dict, labels: dict, broker: BrokerProcess) -> None:
    """Stops at the proposal: a qualified winner is reported, never activated."""
    OUTPUT.mkdir(parents=True, exist_ok=True)
    campaign = record.get("id", "campaign")
    path = OUTPUT / f"{campaign}.json"
    path.write_text(json.dumps(record, indent=2))
    current = state()
    measured = current.get("measured", {})
    rows = []
    for candidate in record.get("candidates", []):
        label = labels.get(candidate.get("proposal_candidate_id", ""), candidate["id"][:8])
        metrics = measured.get(candidate["id"], {})
        quality = candidate.get("quality") or {}
        tps = metrics.get("output_tps")
        rows.append({"label": label, "candidate_id": candidate["id"], "state": candidate.get("state"), "failure_code": candidate.get("failure_code"),
                     "output_tokens_per_second": tps, "request_throughput": metrics.get("request_throughput"), "ttft_p95_ms": metrics.get("ttft_p95_ms"),
                     "latency_p95_ms": metrics.get("latency_p95_ms"), "tpot_p95_ms": metrics.get("tpot_p95_ms"),
                     "usd_per_million_output_tokens": (H100_HOURLY_USD / (tps * 3600) * 1_000_000) if tps else None,
                     "answer_match": quality.get("score"), "answer_check": quality.get("state"), "answer_check_source": quality.get("source"),
                     "comparison": metrics.get("comparison"), "runs": metrics.get("profile_runs"), "requests_per_run": metrics.get("request_count_per_run"),
                     "concurrency": metrics.get("concurrency"), "succeeded": metrics.get("succeeded"), "failed": metrics.get("failed")})
    winner = next((row for row in rows[1:] if row["state"] == "qualified"), None)
    summary = {"schema_version": "infercrane.poc-modal-result/v1", "campaign_id": campaign, "campaign_state": record.get("state"),
               "decision": f"propose {winner['label']}" if winner else "keep the current setup",
               "model": {"id": MODEL_ID, "revision": MODEL_REVISION}, "runtime_image": VLLM_IMAGE, "gpu": "1 x NVIDIA H100 SXM5 80GB on Modal (gpu=\"H100!\")",
               "rate": {"usd_per_hour": H100_HOURLY_USD, "basis": "Modal list price, H100 SXM5, GPU only, read 2026-11-20"},
               "load": {"profile": PROFILE, "objective": OBJECTIVE, "trace": "Mooncake FAST'25 arxiv trace, first rows, inputs scaled 1/2, capped",
                        "traffic": current.get("traffic")},
               "measured_from": "the control plane in a kind cluster on the laptop; every request crosses the internet to Modal through a tunnel",
               "diagnosis": current.get("diagnosis"), "setups": rows, "network": current.get("network"), "broker": broker.ledger()}
    (OUTPUT / f"{campaign}-result.json").write_text(json.dumps(summary, indent=2))
    lines = [f"Traffic: Mooncake (Kimi) production trace replay · model {MODEL_ID} · vLLM 0.22.1 · one H100 per setup on Modal", ""]
    for row in rows:
        tps, cost = row["output_tokens_per_second"], row["usd_per_million_output_tokens"]
        match = "-" if row["answer_match"] is None else f"{row['answer_match']:.0%}"
        lines.append(row["label"])
        lines.append(f"   {('%.0f' % tps) if tps else '-'} output tok/s · p95 latency {('%.0f ms' % row['latency_p95_ms']) if row['latency_p95_ms'] else '-'} · "
                     f"p95 TTFT {('%.0f ms' % row['ttft_p95_ms']) if row['ttft_p95_ms'] else '-'} · ${('%.3f' % cost) if cost else '-'} per 1M output tokens · answers {match}")
        if row is not rows[0]:
            lines.append(f"   {poc.words(row['state'] or '', row['failure_code'])}")
    lines += ["", f"Decision: {summary['decision']}. Nothing is activated (the run stops at the proposal).",
              f"Costs use Modal's H100 list price (${H100_HOURLY_USD}/h, GPU only). Latency includes the laptop-to-Modal network path.",
              f"Full record: {path}"]
    width = max(len(line) for line in lines) + 2
    print("\n╭" + "─" * width + "╮")
    for line in lines:
        print("│ " + line.ljust(width - 1) + "│")
    print("╰" + "─" * width + "╯\n")
    save({"result": lines})


# ---------------------------------------------------------------- download, dry, down


def modal_run(*args: str) -> str:
    return sh("uv", "run", "--quiet", "--no-project", "--with", MODAL_CLIENT, "modal", "run", *args, capture=True)


def download() -> None:
    log("weights", f"staging {MODEL_ID}@{MODEL_REVISION[:12]} on the Modal volume (CPU container, no GPU)")
    out = modal_run(f"{HERE / 'modal_gpu.py'}::main", "--action", "download")
    log("weights", out.strip().splitlines()[-1] if out.strip() else "done")
    (STATE_DIR / "weights.json").write_text(out)


def dry() -> None:
    """One lease end to end on a CPU stand-in: broker, Modal tunnel, the proxy binary, and a
    streamed chat completion through it. Proves the path before any GPU runs."""
    if not state().get("api_key"):
        save({"api_key": __import__("secrets").token_hex(24)})
    binary = STATE_DIR / "bin" / "modal-gpu-proxy"
    binary.parent.mkdir(parents=True, exist_ok=True)
    sh("go", "build", "-o", str(binary), "./internal/testtools/modal-gpu-proxy", quiet=True)
    broker = BrokerProcess(0.25, dry=True, max_starts=2)
    proxy = None
    try:
        env = dict(os.environ, INFERCRANE_WORKER_API_KEY=state()["api_key"])
        proxy = subprocess.Popen([str(binary), "--broker", f"http://127.0.0.1:{BROKER_PORT}", "--port", "18099", "--model", MODEL_ID, "--", "--served-model-name", MODEL_ID],
                                 env=env, stdout=(STATE_DIR / "proxy-dry.log").open("w"), stderr=subprocess.STDOUT)
        for _ in range(180):
            try:
                with urllib.request.urlopen("http://127.0.0.1:18099/health", timeout=5) as response:
                    if response.status == 200:
                        break
            except (urllib.error.URLError, OSError):
                time.sleep(2)
        else:
            fail(f"the dry lease never became healthy: {(STATE_DIR / 'proxy-dry.log').read_text()[-1500:]}")
        body = {"model": MODEL_ID, "stream": True, "messages": [{"role": "user", "content": "hi"}]}
        request = urllib.request.Request("http://127.0.0.1:18099/v1/chat/completions", data=json.dumps(body).encode(), method="POST")
        request.add_header("Authorization", f"Bearer {state()['api_key']}")
        request.add_header("Content-Type", "application/json")
        with urllib.request.urlopen(request, timeout=60) as response:
            streamed = response.read().decode()
        if "[DONE]" not in streamed:
            fail(f"streamed response incomplete: {streamed[:300]}")
        stats = json.loads(urllib.request.urlopen("http://127.0.0.1:18099/proxy/stats", timeout=5).read())
        log("dry", f"lease served a streamed completion through the tunnel; health round trip p50 {stats['health_rtt_p50_ms']} ms over {stats['health_rtt_samples']} checks")
    finally:
        if proxy is not None:
            proxy.send_signal(signal.SIGTERM)
            try:
                proxy.wait(timeout=30)
            except subprocess.TimeoutExpired:
                proxy.kill()
        ledger = broker.stop()
        (STATE_DIR / "dry-ledger.json").write_text(json.dumps(ledger, indent=2))
        states = [lease["state"] for lease in ledger.get("leases", [])]
        log("dry", f"leases: {states}; estimated ${ledger.get('estimated_usd', 0):.4f}")


def stop_gpus() -> None:
    pid = state().get("broker_pid")
    if pid:
        try:
            os.killpg(pid, signal.SIGTERM)
        except OSError:
            pass
    log("broker", "stopped; its ephemeral Modal app stops every container it started")


def down() -> None:
    stop_gpus()
    poc.down()


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    parser.add_argument("command", choices=("up", "download", "dry", "run", "stop-gpus", "down"))
    parser.add_argument("--budget-usd", type=float, default=9.0, help="hard cap on the run's remote GPU spend (estimate from Modal list prices)")
    parser.add_argument("--requests", type=int, default=200, help="Mooncake rows to replay through the gateway")
    parser.add_argument("--reuse-capture", action="store_true", help="skip the baseline and replay; campaign on the traffic shape the last run recorded")
    args = parser.parse_args()
    os.chdir(ROOT)
    STATE_DIR.mkdir(parents=True, exist_ok=True)
    {"up": up, "download": download, "dry": dry, "stop-gpus": stop_gpus, "down": down}.get(args.command, lambda: run_campaign(args.budget_usd, args.requests, args.reuse_capture))()
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
