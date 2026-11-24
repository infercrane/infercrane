#!/usr/bin/env python3
"""One optimization campaign through the real InferCrane control plane on Modal H100s.

    poc_modal.py choose-model  rank the most downloaded FP8 checkpoints from Qwen, Z.ai and
                               DeepSeek that fit one H100 and have a published one-H100 vLLM
                               command, and say whether the pinned model is still the pick
    poc_modal.py up            kind cluster, local registry, model server proxy image, Postgres and
                               `infercrane serve` (Kubernetes provider) in the cluster. No GPU.
    poc_modal.py download      stage the pinned weights on a Modal volume (CPU only)
    poc_modal.py dry           one lease through the broker, tunnel and proxy on a Modal CPU
                               container with a stand-in server (no GPU)
    poc_modal.py run           the publisher's command on an H100 as the current setup, Mooncake
                               replay through the gateway; while it runs, the product's planner
                               chooses what to try from the endpoint's own diagnosis; Replay
                               capture; a campaign of the current setup, the planner's first
                               choice, and a copy of the current setup that must not win; the
                               built-in answer check and ranking. Stops at the proposal.
    poc_modal.py down          delete the cluster and registry

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
import threading
import time
import urllib.error
import urllib.parse
import urllib.request
from pathlib import Path

HERE = Path(__file__).resolve().parent
sys.path.insert(0, str(HERE.parent / "poc-local"))
sys.path.insert(0, str(HERE))
import models  # noqa: E402
import poc  # noqa: E402

ROOT = poc.ROOT
MODAL_CLIENT = "modal==1.6.4"
# The same table the GPU side enforces; the broker refuses any other model or argument.
MODEL = models.QWEN38_27B_FP8
MODEL_ID, MODEL_REVISION = MODEL.id, MODEL.revision
VLLM_IMAGE = "vllm/vllm-openai@sha256:953d3a06d5e64ab582985cd7401289d3abf2a2c14ef2158e9a84313daeec77d7"
ENGINE_VERSION = "vllm-openai-0.22.1-953d3a06"
# Modal list price for one H100 SXM5, $0.001097 per second (modal.com/pricing, read
# 2026-11-20). GPU only: the container's CPU and memory are billed on top and are in the
# broker's estimate and Modal's billing report, not in this rate.
H100_HOURLY_USD = round(0.001097 * 3600, 4)
APPROVAL_HOURS = 3
BROKER_PORT = 18095
BROKER_HOST = "host.docker.internal"  # the Mac host as seen from kind pods (Docker Desktop)
PROFILE, OBJECTIVE = "throughput", "throughput"  # 32 concurrent streams per measured run
MAX_INPUT_TOKENS = 30000  # the traffic shape of the 2026-11-20 run; Qwen3.8 serves 262,144 positions

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

CONTROL = ("Your current setup (the publisher's vLLM command)", "publisher-command", MODEL.current_args)
# The same setup again on its own server. It must never win: if it does, the judge crowned
# a difference between two machines, and the run proves nothing. Only the sampling seed
# differs, which gives it its own identity and does not change its speed.
COPY = ("A copy of your current setup (must not win)", "publisher-command-copy", [*MODEL.current_args, "--seed", "1"])
# The broker runs three servers at once: the current setup, the planner's first choice, and the copy.
PLANNER_SETUPS = 1
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
    spec = {"apiVersion": "infercrane.dev/v1", "kind": "Deployment", "name": poc.BASELINE, "model": {"id": MODEL_ID, "revision": MODEL_REVISION}, "runtime": runtime(list(CONTROL[2])),
            "compute": {"mode": "elastic"}, "resources": {"gpu": poc.ACCELERATOR, "gpu_count": 1}, "provider": {"cloud": "kubernetes", "adapter": "kubernetes", "region": ""},
            "scaling": {"min_replicas": 1, "max_replicas": 1}, "routing": {"strategy": "round-robin"}}
    path = STATE_DIR / "baseline.yaml"
    path.write_text(json.dumps(spec, indent=2))
    digest = hashlib.sha256(json.dumps(spec, sort_keys=True).encode()).hexdigest()[:12]
    log("deploy", f"current setup: {MODEL_ID} with its publisher's vLLM command ({' '.join(CONTROL[2])}), one H100 ({poc.BASELINE}); first start loads the image and weights")
    poc.cli("deploy", str(path), "--idempotency-key", f"{poc.BASELINE}-{digest}", "--wait", capture=False)


def replay_mooncake(requests_total: int, input_scale: int, max_output: int, workers: int, progress: dict) -> dict:
    """Replays the first rows of the Mooncake trace through the gateway at their recorded
    arrival times. Inputs are scaled 1/input_scale and capped to fit the model. progress
    counts the requests that finished, for the planner waiting on a loaded server."""
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
        finally:
            progress["done"] = progress.get("done", 0) + 1
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


# Engine measurements that exist only once two scrapes of the loaded server are compared.
WINDOWED = {"runtime_requests_completed", "runtime_queue_time_p95", "runtime_prefill_time_p95", "runtime_decode_time_p95"}


def engine_measured(document: object) -> bool:
    """True once endpoint monitoring holds a windowed engine measurement."""
    stack = [document]
    while stack:
        item = stack.pop()
        if isinstance(item, dict):
            if item.get("name") in WINDOWED and item.get("availability") == "available":
                return True
            stack.extend(item.values())
        elif isinstance(item, list):
            stack.extend(item)
    return False


class Planner:
    """Asks the product's planner what to try, from the current setup's own diagnosis, the
    way a customer does with `optimize propose --diagnosis-endpoint`. It asks while the
    replayed traffic still runs, once the control plane's engine scrape (every 30 s) has
    windowed measurements and half the requests have finished, so the diagnosis sees the
    loaded server's queue, KV cache and timings rather than an idle one. When the traffic
    ends first, it waits up to 90 seconds for that evidence and then asks anyway."""

    def __init__(self, total: int) -> None:
        self.total, self.progress, self.replay_done = total, {"done": 0}, threading.Event()
        self.proposal: dict | None = None
        self.asked: str = ""
        self.error: str = ""
        self.thread = threading.Thread(target=self.run, daemon=True)
        self.thread.start()

    def run(self) -> None:
        ended_at = None
        try:
            while True:
                try:
                    measured = engine_measured(poc.api("GET", f"/api/v1/endpoints/{poc.BASELINE}/monitoring"))
                except SystemExit:
                    measured = False
                loaded = self.progress["done"] >= max(20, self.total // 2)
                if self.replay_done.is_set():
                    ended_at = ended_at or time.monotonic()
                if measured and loaded and not self.replay_done.is_set():
                    self.asked = f"while the traffic ran, after {self.progress['done']} of {self.total} requests finished"
                    break
                if ended_at and (measured or time.monotonic() - ended_at > 90):
                    self.asked = "after the traffic ended" + ("" if measured else ", without windowed engine measurements")
                    break
                time.sleep(10)
            self.proposal = json.loads(poc.cli("optimize", "propose", MODEL_ID, "--model-revision", MODEL_REVISION, "--provider", "kubernetes", "--gpu", poc.ACCELERATOR,
                                               "--runtimes", "vllm", "--objective", OBJECTIVE, "--profile", PROFILE, "--max-candidates", "6",
                                               "--diagnosis-endpoint", poc.BASELINE, "--include-simulated", "--output", "json"))
        except (SystemExit, Exception) as failure:  # noqa: BLE001 - reported by finish()
            self.error = f"{type(failure).__name__}: {failure}"

    def finish(self) -> dict:
        self.replay_done.set()
        self.thread.join(timeout=300)
        if self.proposal is None:
            fail(f"the planner returned no proposal: {self.error or 'it did not answer within 5 minutes'}")
        log("plan", f"asked the planner {self.asked}")
        return self.proposal


def plan(proposal: dict) -> list[dict]:
    """Turns the planner's setups into campaign setups. Each of the planner's setups is its
    starting setup with one change, chosen by a diagnosis rule; the same change is applied
    to the publisher's command. A setup the remote GPU cannot run as measured is skipped
    with the reason, and the copy of the current setup is added last."""
    diagnosis = proposal.get("diagnosis") or {}
    log("diagnose", f"{diagnosis.get('state', 'no diagnosis')}: {diagnosis.get('summary') or diagnosis.get('primary_bottleneck') or 'the planner recorded no summary'}")
    start_id = (proposal.get("starting_point") or {}).get("candidate_id")
    start = next((c for c in proposal.get("candidates", []) if c["id"] == start_id), None)
    if start is None:
        fail(f"the planner's proposal has no starting setup: {json.dumps(proposal)[:600]}")
    start_runtime = start["deployment"]["runtime"]
    record, chosen = [], [CONTROL]
    for candidate in proposal["candidates"]:
        if candidate["id"] == start_id:
            continue
        reason, deployment = candidate.get("reason") or {}, candidate["deployment"]
        runtime_doc = deployment["runtime"]
        args = models.with_change(CONTROL[2], start_runtime.get("args") or [], runtime_doc.get("args") or [])
        row = {"label": reason.get("label") or candidate.get("configuration_profile"), "rules": reason.get("rules", []), "summary": reason.get("summary", ""),
               "evidence": reason.get("evidence", []), "planner_args": runtime_doc.get("args") or [], "args": args}
        skipped = ""
        if (deployment.get("resources") or {}).get("gpu_count", 1) != 1 or (deployment.get("provider") or {}).get("adapter") != "kubernetes":
            skipped = "it needs more than one GPU or another provider adapter"
        elif (runtime_doc.get("env") or {}) != (start_runtime.get("env") or {}):
            skipped = "it changes the engine's environment, which every setup of a run shares"
        elif any(flag == "--speculative-config" for flag, _ in models.parse(args)):
            skipped = "speculative decoding: the replayed traffic has no real text, so draft acceptance would not be honest"
        elif args == list(CONTROL[2]):
            skipped = "the current setup already makes this change"
        elif MODEL.max_num_seqs and int(dict(models.parse(args)).get("--max-num-seqs") or 0) > MODEL.max_num_seqs:
            skipped = f"vLLM cannot start it on one H100: {MODEL.id} fits at most {MODEL.max_num_seqs} requests at once"
        elif len(chosen) > PLANNER_SETUPS:
            skipped = f"only the planner's first {PLANNER_SETUPS} setup fits next to the current setup and its copy"
        else:
            try:
                models.check_args(["--served-model-name", MODEL_ID, *args], MODEL_ID)
            except ValueError as error:
                skipped = f"the remote GPU refuses it: {error}"
        row["skipped"] = skipped
        record.append(row)
        if skipped:
            log("plan", f"skip “{row['label']}”: {skipped}")
            continue
        chosen.append((row["label"], candidate.get("configuration_profile") or "planner-setup", args))
        log("plan", f"try “{row['label']}”: {row['summary'] or ', '.join(row['rules'])}  [change: {' '.join(row['planner_args'])}]")
    if len(chosen) > 1:
        chosen.append(COPY)
        log("plan", f"also measure “{COPY[0]}”: it must not win, or the run proves nothing")
    CANDIDATES[:] = chosen
    save({"plan": [{"label": label, "profile": profile, "args": args} for label, profile, args in chosen],
          "planner": {"diagnosis": diagnosis, "input_diagnosis": (proposal.get("input") or {}).get("diagnosis"), "setups": record}})
    return record


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
    measured = workload_doc.get("measured") or {}
    out |= {"output_tps": row.get("output_token_throughput"), "comparison": measured.get("comparison") or {},
            "confirmation": (measured.get("confirmation") or {}).get("comparison"),
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
    publisher_check()
    if reuse_capture and not (previous.get("capture_digest") and previous.get("plan") and previous.get("diagnosis")):
        fail("--reuse-capture needs an earlier run's recorded traffic shape and diagnosis")
    poc.port_forward()
    poc.reset_previous()  # clears the plan and diagnosis; a reused capture keeps its own
    kept = {key: previous.get(key) for key in ("capture_digest", "plan", "diagnosis", "traffic", "planner")} if reuse_capture else {"traffic": None, "capture_digest": None, "planner": None}
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
            planner = Planner(requests_total)
            traffic = replay_mooncake(requests_total, input_scale=2, max_output=512, workers=96, progress=planner.progress)
            save({"traffic": traffic})
            plan(planner.finish())
            diagnosis = state().get("planner")
            digest = poc.capture_replay()
            save({"capture_digest": digest})
            log("deploy", f"removing {poc.BASELINE}: its traffic shape is recorded, and its H100 is not needed for the campaign")
            poc.cli("delete", poc.BASELINE, "--yes", "--wait", check=False)
            wait_for_lease_end(broker, poc.BASELINE)
        if len(CANDIDATES) < 2:
            log("plan", "the planner chose nothing the remote GPU can measure for this traffic; no campaign runs")
            finish({"id": f"no-campaign-{state()['run_id']}", "state": "not_started", "candidates": []}, {}, broker)
            return
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


def objective_decision(row: dict) -> str | None:
    """The decision on the objective metric that counts: the measurement on fresh servers
    when there was one, otherwise the first window's."""
    metric = "output_token_throughput"
    for comparison in (row.get("confirmation"), row.get("comparison")):
        if comparison and metric in comparison:
            return comparison[metric].get("decision")
    return None


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
                     "comparison": metrics.get("comparison"), "confirmation": metrics.get("confirmation"), "runs": metrics.get("profile_runs"), "requests_per_run": metrics.get("request_count_per_run"),
                     "concurrency": metrics.get("concurrency"), "succeeded": metrics.get("succeeded"), "failed": metrics.get("failed")})
    copy = next((row for row in rows if row["label"] == COPY[0]), None)
    copy_won = bool(copy and (copy["state"] == "qualified" or objective_decision(copy) == "improved"))
    winner = next((row for row in rows[1:] if row["state"] == "qualified" and row is not copy), None)
    decision = f"propose {winner['label']}" if winner else "keep the current setup"
    if record.get("state") == "not_started":
        decision = "no campaign: the planner chose nothing the remote GPU can measure for this traffic"
    if copy_won:
        decision = "no decision: the copy of the current setup measured better than the current setup, so the judge crowned a machine difference"
    summary = {"schema_version": "infercrane.poc-modal-result/v2", "campaign_id": campaign, "campaign_state": record.get("state"), "decision": decision,
               "copy_of_current_setup": {"must_not_win": True, "won": copy_won, "state": copy and copy["state"], "failure_code": copy and copy["failure_code"],
                                         "objective_decision": copy and objective_decision(copy)} if copy else None,
               "model": {"id": MODEL_ID, "revision": MODEL_REVISION, "chosen_by": "poc_modal.py choose-model: most downloaded FP8 checkpoint from Qwen, Z.ai and DeepSeek that fits one H100"},
               "current_setup": {"args": CONTROL[2], "env": MODEL.env, "publisher_args": list(MODEL.publisher_args), "publisher_env": MODEL.publisher_env,
                                 "added_args": list(MODEL.added_args), "added_because": MODEL.added_because, "source": MODEL.source}, "planner": current.get("planner"),
               "runtime_image": VLLM_IMAGE, "gpu": "1 x NVIDIA H100 SXM5 80GB on Modal (gpu=\"H100!\")",
               "rate": {"usd_per_hour": H100_HOURLY_USD, "basis": "Modal list price, H100 SXM5, GPU only, read 2026-11-20"},
               "load": {"profile": PROFILE, "objective": OBJECTIVE, "trace": "Mooncake FAST'25 arxiv trace, first rows, inputs scaled 1/2, capped",
                        "traffic": current.get("traffic")},
               "measured_from": "the control plane in a kind cluster on the laptop; every request crosses the internet to Modal through a tunnel",
               "diagnosis": current.get("diagnosis"), "setups": rows, "network": current.get("network"), "broker": broker.ledger()}
    (OUTPUT / f"{campaign}-result.json").write_text(json.dumps(summary, indent=2))
    lines = [f"Traffic: Mooncake (Kimi) production trace replay · model {MODEL_ID} · vLLM 0.22.1 · one H100 per setup on Modal", ""]
    if copy:
        lines += [("The copy of the current setup measured better: this run proves nothing." if copy_won else
                   "The copy of the current setup did not win, as it must not."), ""]
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


# ---------------------------------------------------------------- choose-model


PUBLISHERS = ("Qwen", "zai-org", "deepseek-ai")
# Weights may take this share of an H100's 80 GB; the rest is KV cache and activations.
WEIGHT_SHARE = 0.6
DTYPE_BYTES = {"F8_E4M3": 1, "F8_E5M2": 1, "BF16": 2, "F16": 2, "F32": 4, "I8": 1, "U8": 1, "I32": 4}


def get_json(url: str) -> object:
    request = urllib.request.Request(url, headers={"User-Agent": "infercrane-poc-modal"})
    with urllib.request.urlopen(request, timeout=60) as response:
        return json.loads(response.read())


def recipe_url(model_id: str) -> str:
    """vLLM's recipe for one H100 in FP8; recipes are named after the base checkpoint."""
    return f"https://recipes.vllm.ai/{model_id.removesuffix('-FP8')}.json?" + urllib.parse.urlencode({"hardware": "h100", "variant": "fp8", "strategy": "single_node_tp"})


def one_h100_command(model_id: str) -> tuple[list[str], dict] | None:
    """The publisher's vLLM arguments and environment for one H100, or None when vLLM's
    recipes have no such command without speculative decoding."""
    try:
        recipe = get_json(recipe_url(model_id))
    except (urllib.error.URLError, OSError, json.JSONDecodeError):
        return None
    argv = [str(token) for token in recipe.get("argv") or []]
    if recipe.get("hardware") != "h100" or recipe.get("variant") != "fp8" or argv[:3] != ["vllm", "serve", model_id]:
        return None  # an unknown configuration falls back to the recipe's default
    args = argv[3:]
    if dict(models.parse(args)).get("--tensor-parallel-size", "1") != "1" or any(flag == "--speculative-config" for flag, _ in models.parse(args)):
        return None
    return args, {str(key): str(value) for key, value in (recipe.get("env") or {}).items()}


def choose_model() -> None:
    rows = []
    for author in PUBLISHERS:
        rows += get_json(f"https://huggingface.co/api/models?author={author}&search=FP8&sort=downloads&direction=-1&limit=20")
    rows.sort(key=lambda row: row.get("downloads", 0), reverse=True)
    pick = None
    for row in rows[:12]:
        info = get_json(f"https://huggingface.co/api/models/{row['id']}?expand[]=safetensors&expand[]=sha")
        parameters = ((info.get("safetensors") or {}).get("parameters") or {})
        size = sum(count * DTYPE_BYTES.get(dtype, 2) for dtype, count in parameters.items())
        fits = 0 < size <= WEIGHT_SHARE * 80e9
        command = one_h100_command(row["id"]) if fits else None
        verdict = "pick" if fits and command and pick is None else ("fits, has a one-H100 command" if fits and command else
                                                                       "no published one-H100 vLLM command without speculation found" if fits else "does not fit one H100")
        if verdict == "pick":
            pick = (row["id"], info.get("sha"), command)
        print(f"{row.get('downloads', 0):>10,}  {row['id']:<48} {size / 1e9:6.1f} GB  {verdict}")
    if pick is None:
        fail("no checkpoint fits one H100 with a published one-H100 vLLM command")
    model_id, revision, (args, env) = pick
    pinned = model_id == MODEL_ID and args == list(MODEL.publisher_args) and env == (MODEL.publisher_env or {})
    log("model", f"pick: {model_id}@{revision} · {' '.join(args)} · env {env}")
    log("model", "the pinned model and command are still the pick" if pinned else
        f"the pin differs: models.py has {MODEL_ID}@{MODEL_REVISION[:12]}; update it and stage the weights before a run")


def publisher_check() -> None:
    """Warns when the publisher's command changed since it was pinned. The run uses the
    pinned command either way, and its record says which."""
    command = one_h100_command(MODEL_ID)
    if command is None:
        log("model", f"could not read the publisher's one-H100 command now; running the pinned one from {MODEL.source}")
    elif command != (list(MODEL.publisher_args), MODEL.publisher_env or {}):
        log("model", f"the publisher's command changed since it was pinned ({' '.join(command[0])}, env {command[1]}); running the pinned one")
    save({"publisher_command_now": command and {"args": command[0], "env": command[1]}})


# ---------------------------------------------------------------- download, dry, down


def modal_run(*args: str) -> str:
    return sh("uv", "run", "--quiet", "--no-project", "--with", MODAL_CLIENT, "modal", "run", *args, capture=True)


def download() -> None:
    log("weights", f"staging {MODEL_ID}@{MODEL_REVISION[:12]} on the Modal volume (CPU container, no GPU)")
    out = modal_run(f"{HERE / 'modal_gpu.py'}::main", "--action", "download", "--model", MODEL_ID)
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
    parser.add_argument("command", choices=("choose-model", "up", "download", "dry", "run", "stop-gpus", "down"))
    parser.add_argument("--budget-usd", type=float, default=9.0, help="hard cap on the run's remote GPU spend (estimate from Modal list prices)")
    parser.add_argument("--requests", type=int, default=200, help="Mooncake rows to replay through the gateway")
    parser.add_argument("--reuse-capture", action="store_true", help="skip the baseline and replay; campaign on the traffic shape the last run recorded")
    args = parser.parse_args()
    os.chdir(ROOT)
    STATE_DIR.mkdir(parents=True, exist_ok=True)
    {"choose-model": choose_model, "up": up, "download": download, "dry": dry, "stop-gpus": stop_gpus, "down": down}.get(args.command, lambda: run_campaign(args.budget_usd, args.requests, args.reuse_capture))()
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
