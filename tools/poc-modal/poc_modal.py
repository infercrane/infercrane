#!/usr/bin/env python3
"""One optimization campaign through the real InferCrane control plane on Modal GPUs.

    poc_modal.py choose-model  rank the most downloaded FP8 checkpoints from Qwen, Z.ai and
                               DeepSeek that fit one H100 and have a published one-H100 vLLM
                               command, and say whether the pinned model is still the pick
    poc_modal.py up            kind cluster, local registry, model server proxy image, Postgres and
                               `infercrane serve` (Kubernetes provider) in the cluster. No GPU.
    poc_modal.py download      stage the pinned weights on a Modal volume (CPU only)
    poc_modal.py dry           one lease through the broker, tunnel and proxy on a Modal CPU
                               container with a stand-in server (no GPU)
    poc_modal.py run           the publisher's command on a GPU as the current setup, Mooncake
                               replay through the gateway; while it runs, the product's planner
                               chooses what to try from the endpoint's own diagnosis; Replay
                               capture; a campaign of the current setup, the planner's choice,
                               and a copy of the current setup that must not win; the built-in
                               answer check and ranking. Stops at the proposal.
                               --traffic real-text composes every request from a prompt corpus
                               of real text, so speculative setups are measured, not skipped.
                               --smoke shrinks the traffic so the whole path costs a few dollars.
                               --stand-in runs the same path on CPU stand-in servers for cents.
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
import corpus as prompt_corpus  # noqa: E402
import models  # noqa: E402
import poc  # noqa: E402

ROOT = poc.ROOT
MODAL_CLIENT = "modal==1.6.4"
# The same table the GPU side enforces; the broker refuses any other model or argument.
MODEL = models.QWEN38_27B_FP8
MODEL_ID, MODEL_REVISION = MODEL.id, MODEL.revision
APPROVAL_HOURS = 3
BROKER_PORT = 18095
BROKER_HOST = "host.docker.internal"  # the Mac host as seen from kind pods (Docker Desktop)
PROFILE, OBJECTIVE = "throughput", "throughput"  # 32 concurrent streams per measured run
# The requests at once a campaign measures with are its benchmark profile's. A campaign
# bound to recorded traffic takes nothing else from the profile, so these three built-in
# profiles are three load levels; the objective stays output throughput. The product has
# no profile with 12 at once.
LOAD_PROFILES = {32: "throughput", 8: "balanced", 4: "long-context"}
MAX_INPUT_TOKENS = 30000  # the traffic shape of the 2026-11-20 run; Qwen3.8 serves 262,144 positions
# A smoke run proves a paid campaign's path for a few dollars. The judge keeps its four
# paired rounds and its fresh-server confirmation (the product has no shorter mode: with
# one pair it cannot decide), so the traffic shrinks instead: fewer and shorter requests,
# spread over long enough for the control plane's engine scrape (every 30 s) to see a
# loaded server twice.
SMOKE = {"requests": 40, "max_input_tokens": 6000, "max_output_tokens": 128, "spread_seconds": 150.0}
# Traffic shapes: the median input and output tokens of a request. "mooncake" is the trace
# as the 2026-11-20 run replayed it (inputs halved, outputs capped at 512). The other three
# are the product's public workload shapes, medians of the Chutes data (CC BY 4.0;
# internal/performanceprofile public-interactive, public-long-prefill, public-decode-heavy).
# The trace's rows are scaled so their medians land there; its spread, arrival times and
# shared prefixes stay.
SHAPES = {"mooncake": None, "interactive": (3919, 295), "long-prefill": (7897, 295), "decode-heavy": (3919, 1377)}


def shape_scales(shape: str, rows: list[dict]) -> dict:
    """How to scale the trace's rows for a traffic shape: divisors for input and output
    lengths, and the cap on output tokens."""
    if SHAPES[shape] is None:
        return {"input_scale": 2.0, "output_scale": 1.0, "max_output_tokens": 512}
    median = lambda values: sorted(values)[len(values) // 2]  # noqa: E731
    target_input, target_output = SHAPES[shape]
    return {"input_scale": median([row["input_length"] for row in rows]) / target_input, "output_scale": median([row["output_length"] for row in rows]) / target_output,
            "max_output_tokens": min(4096, 4 * target_output)}

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
poc.APPROVAL_HOURS = APPROVAL_HOURS
poc.API_PORT = 18081
poc.BASELINE = "gpu-baseline"
poc.OPTIMIZED = "gpu-optimized"
poc.MODEL_ID = MODEL_ID
poc.MODEL_REVISION = MODEL_REVISION
poc.MOONCAKE_BLOCK_TOKENS = 512
STATE_DIR, OUTPUT, log, fail = poc.STATE_DIR, poc.OUTPUT, poc.log, poc.fail
LEDGER = STATE_DIR / "broker-ledger.json"

CURRENT_SETUPS = ("publisher", "publisher-speculation")
TRAFFIC = ("mooncake-words", "real-text")
# Stated in every result, so its tokens per second are not read as another run's.
REASONING_NOTE = ("Requests do not switch the model's reasoning off, so it reasons before it answers, as a request without that switch does in production. "
                  "Output tokens per second count every generated token, reasoning included. The built-in answer check does switch reasoning off for its "
                  "24 short prompts and scores word overlap with the current setup's answers (token F1, at most 64 tokens each), not exact text.")
DOCUMENTED_LABEL = "The publisher's documented speculation option"
# The broker runs four servers at once: the current setup, two setups to try, and the copy.
CANDIDATE_SLOTS = 2
CANDIDATES: list = []
# What this run measures with. Set by configure() and by `run`'s flags.
RUN = {"traffic": "mooncake-words", "shape": "mooncake", "corpus": None, "smoke": False, "stand_in": False, "current_setup": "publisher", "planner_setups": 1}


def configure(engine: str | None = None, gpu: str | None = None, current_setup: str = "publisher") -> None:
    """Sets the run's engine image, GPU type and current setup. The GPU rate is Modal's
    list price for the GPU alone: the container's CPU and memory are billed on top and are
    in the broker's estimate and Modal's billing report, not in this rate."""
    global VLLM_IMAGE, ENGINE_VERSION, GPU_HOURLY_USD, CONTROL, COPY
    models.select(engine or models.ENGINE.key, gpu or models.ACCELERATOR.key)
    VLLM_IMAGE, ENGINE_VERSION = models.ENGINE.image, models.ENGINE.label
    GPU_HOURLY_USD = round(models.ACCELERATOR.usd_per_second * 3600, 4)
    poc.ACCELERATOR = models.ACCELERATOR.key
    poc.ACCELERATOR_RESOURCE = f"infercrane.dev/modal-{models.ACCELERATOR.key.lower()}-lease"
    poc.NOMINAL_HOURLY_USD = GPU_HOURLY_USD
    RUN["current_setup"] = current_setup
    if current_setup == "publisher-speculation":
        if not MODEL.publisher_speculation:
            fail(f"{MODEL_ID} has no publisher-documented speculation option in models.py")
        CONTROL = ("Your current setup (the publisher's vLLM command with its documented speculation option)", "publisher-speculation", MODEL.speculation_args)
    else:
        CONTROL = ("Your current setup (the publisher's vLLM command)", "publisher-command", MODEL.current_args)
    # The same setup again on its own server. It must never win: if it does, the judge
    # crowned a difference between two machines, and the run proves nothing. Only the
    # sampling seed differs, which gives it its own identity and does not change its speed.
    COPY = ("A copy of your current setup (must not win)", f"{CONTROL[1]}-copy", [*CONTROL[2], "--seed", "1"])
    CANDIDATES[:] = [CONTROL]


configure()


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
    """The node advertises one schedulable slot per remote GPU the broker may run, labeled
    with the GPU type the work runs on; the resource name says it is a Modal lease."""
    poc.kubectl("label", "node", poc.NODE, f"{poc.ACCELERATOR_LABEL}={poc.ACCELERATOR}", "--overwrite")
    escaped = poc.ACCELERATOR_RESOURCE.replace("~", "~0").replace("/", "~1")
    patch = json.dumps([{"op": "add", "path": f"/status/capacity/{escaped}", "value": str(count)},
                        {"op": "add", "path": f"/status/allocatable/{escaped}", "value": str(count)}])
    sh("kubectl", "--context", poc.CONTEXT, "patch", "node", poc.NODE, "--subresource=status", "--type=json", "-p", patch, quiet=True)
    for _ in range(30):
        if poc.ACCELERATOR_RESOURCE.split("/", 1)[1] in poc.kubectl("get", "node", poc.NODE, "-o", "jsonpath={.status.allocatable}"):
            break
        time.sleep(1)
    else:
        fail("the node did not report the Modal lease resource as allocatable")
    log("setup", f"node advertises {count} x {poc.ACCELERATOR_RESOURCE} (one per remote {poc.ACCELERATOR} the broker may run)")


def rate_card() -> str:
    now = dt.datetime.now(dt.timezone.utc).replace(microsecond=0)
    stamp = lambda moment: moment.isoformat().replace("+00:00", "Z")  # noqa: E731
    row = {"cloud": "kubernetes", "region": "", "gpu": poc.ACCELERATOR, "gpu_count": 1, "replicas": 1, "hourly_usd": GPU_HOURLY_USD, "currency": "USD",
           "source": f"operator-rate-card/modal-{poc.ACCELERATOR.lower()}-list-{models.ACCELERATOR.price_read}", "authority": "account_contract", "cost_scope": "instance_total",
           "observed_at": stamp(now - dt.timedelta(minutes=5)), "valid_until": stamp(now + dt.timedelta(days=7))}
    return json.dumps([row], separators=(",", ":"))


# Where the control plane reads the run's prompt corpus: a directory of the kind node,
# mounted read-only into the control plane pod. A ConfigMap would cap the corpus at 1 MiB.
NODE_CORPUS_DIR, POD_CORPUS = "/var/lib/infercrane-poc-corpus", "/etc/infercrane-corpus/prompt-corpus.json"
CORPUS_VARIABLE = "INFERCRANE_REPLAY_PROMPT_CORPUS_FILE"


def mount_corpus(path: Path | None) -> None:
    """Gives the control plane the prompt corpus, or takes it away. With a corpus the
    product composes every replayed campaign request from it; without one AIPerf
    synthesizes the text, as before."""
    if path is None:
        poc.kubectl("-n", poc.NAMESPACE, "set", "env", "deploy/infercrane", f"{CORPUS_VARIABLE}-")
        save({"corpus": None})
        return
    text = prompt_corpus.load(path)
    sh("docker", "exec", poc.NODE, "mkdir", "-p", NODE_CORPUS_DIR, quiet=True)
    sh("docker", "cp", str(path), f"{poc.NODE}:{NODE_CORPUS_DIR}/prompt-corpus.json", quiet=True)
    sh("docker", "exec", poc.NODE, "chmod", "644", f"{NODE_CORPUS_DIR}/prompt-corpus.json", quiet=True)
    patch = {"spec": {"template": {"spec": {
        "volumes": [{"name": "prompt-corpus", "hostPath": {"path": NODE_CORPUS_DIR, "type": "Directory"}}],
        "containers": [{"name": "infercrane", "env": [{"name": CORPUS_VARIABLE, "value": POD_CORPUS}],
                        "volumeMounts": [{"name": "prompt-corpus", "mountPath": str(Path(POD_CORPUS).parent), "readOnly": True}]}]}}}}
    poc.kubectl("-n", poc.NAMESPACE, "patch", "deploy", "infercrane", "--type=strategic", "-p", json.dumps(patch))
    save({"corpus": text.identity})
    log("setup", f"prompt corpus for replay measurement: {text.identity['name']} · {text.identity['license']} · {text.identity['passages']} passages · {text.digest[:19]}…")


def corpus_in_control_plane(text: prompt_corpus.Corpus) -> None:
    """Fails unless the running control plane composes its replay from exactly this
    corpus: the same bytes on the node, and the setting on the deployment."""
    setting = poc.kubectl("-n", poc.NAMESPACE, "get", "deploy", "infercrane", "-o", f"jsonpath={{.spec.template.spec.containers[0].env[?(@.name=='{CORPUS_VARIABLE}')].value}}", check=False).strip()
    digest = sh("docker", "exec", poc.NODE, "sha256sum", f"{NODE_CORPUS_DIR}/prompt-corpus.json", capture=True, check=False).split(" ", 1)[0].strip()
    if setting != POD_CORPUS or f"sha256:{digest}" != text.digest:
        fail(f"the control plane does not hold this prompt corpus ({text.digest[:19]}…); run `poc_modal.py up --corpus {text.path}` first")


def up(corpus: Path | None = None) -> None:
    for tool in ("docker", "kubectl", "go", "curl", "uv"):
        if not shutil.which(tool):
            fail(f"{tool} is required")
    if not state().get("api_key"):
        save({"api_key": __import__("secrets").token_hex(24)})
    kind = poc.kind_binary()
    poc.ensure_registry()
    poc.ensure_cluster(kind)
    advertise_leases(CANDIDATE_SLOTS + 2)
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
    mount_corpus(corpus)
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
        # The broker's port is open before its Modal app is up, and the app's first start
        # with a new engine image unpacks about 10 GB on Modal, so the first answer can take
        # many minutes. No container runs meanwhile, so the wait costs nothing.
        deadline, told = time.monotonic() + 25 * 60, False
        while time.monotonic() < deadline:
            if self.process.poll() is not None:
                fail(f"the broker exited: {self.log_path.read_text()[-1500:]}")
            try:
                self.request("GET", "/v1/ledger", timeout=5)
                return
            except (urllib.error.URLError, OSError):
                if not told and time.monotonic() > deadline - 24 * 60:
                    told = True
                    log("broker", f"waiting for the broker's Modal app (a new engine image is built once; see {self.log_path})")
                time.sleep(2)
        self.process.kill()
        save({"broker_pid": None})
        fail(f"the broker did not start within 25 minutes: {self.log_path.read_text()[-1500:]}")

    def request(self, method: str, path: str, body: dict | None = None, timeout: float = 60) -> dict:
        data = json.dumps(body).encode() if body is not None else None
        request = urllib.request.Request(f"http://127.0.0.1:{BROKER_PORT}{path}", method=method, data=data)
        request.add_header("Authorization", f"Bearer {state()['api_key']}")
        request.add_header("Content-Type", "application/json")
        with urllib.request.urlopen(request, timeout=timeout) as response:
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


def server_kind() -> str:
    return "CPU stand-in" if RUN["stand_in"] else poc.ACCELERATOR


def deploy_baseline() -> None:
    spec = {"apiVersion": "infercrane.dev/v1", "kind": "Deployment", "name": poc.BASELINE, "model": {"id": MODEL_ID, "revision": MODEL_REVISION}, "runtime": runtime(list(CONTROL[2])),
            "compute": {"mode": "elastic"}, "resources": {"gpu": poc.ACCELERATOR, "gpu_count": 1}, "provider": {"cloud": "kubernetes", "adapter": "kubernetes", "region": ""},
            "scaling": {"min_replicas": 1, "max_replicas": 1}, "routing": {"strategy": "round-robin"}}
    path = STATE_DIR / "baseline.yaml"
    path.write_text(json.dumps(spec, indent=2))
    digest = hashlib.sha256(json.dumps(spec, sort_keys=True).encode()).hexdigest()[:12]
    log("deploy", f"current setup: {MODEL_ID} on {ENGINE_VERSION} ({' '.join(CONTROL[2])}), one {server_kind()} ({poc.BASELINE}); first start loads the image and weights")
    # The key names this run. A key of the spec alone made a second run in the same cluster
    # a replay of the first run's deploy, after that run had removed the deployment: nothing
    # was deployed and every replayed request got a 404.
    poc.cli("deploy", str(path), "--idempotency-key", f"{poc.BASELINE}-{digest}-{state()['run_id']}", "--wait", capture=False)


def replay_mooncake(requests_total: int, input_scale: float, max_output: int, workers: int, progress: dict, max_input: int = MAX_INPUT_TOKENS,
                    spread_seconds: float | None = None, text: prompt_corpus.Corpus | None = None, output_scale: float = 1.0) -> dict:
    """Replays the first rows of the Mooncake trace through the gateway at their recorded
    arrival times (stretched over spread_seconds when given). Input lengths are divided
    by input_scale and capped to fit the model, output lengths by output_scale and capped
    at max_output. progress counts the requests that finished, for the planner waiting
    on a loaded server.

    The trace has no content. Without a corpus the prompts are words generated from its
    prefix block hashes and answers are forced to their recorded length. With one, every
    request is real text composed from the corpus (corpus.compose), the same prefix
    blocks share the same passages, and answers end where the model ends them, capped at
    the recorded length."""
    rows = poc.mooncake_rows()[:requests_total]
    start = rows[0]["timestamp"]
    stretch = spread_seconds / max(1.0, (rows[-1]["timestamp"] - start) / 1000) if spread_seconds else 1.0
    block = max(1, round(poc.MOONCAKE_BLOCK_TOKENS / input_scale))
    what = f"real text from the prompt corpus “{text.identity['name']}” ({text.identity['license']})" if text else "prompts are generated words"
    log("traffic", f"replaying {len(rows)} requests from the Mooncake (Kimi) production trace at their recorded arrival times"
        f"{f' stretched over {spread_seconds:.0f} s' if spread_seconds else ''}: shared prefixes kept, inputs divided by {input_scale:.2f} and capped at {max_input} tokens, outputs divided by {output_scale:.2f} and capped at {max_output}; {what}")
    key, began, failures = state()["api_key"], time.monotonic(), []
    sent = {"input_words": 0, "max_tokens": 0, "planned_input_tokens": 0}

    def one(indexed: tuple[int, dict]) -> int:
        index, row = indexed
        delay = (row["timestamp"] - start) / 1000 * stretch - (time.monotonic() - began)
        if delay > 0:
            time.sleep(delay)
        target = min(max_input, max(8, round(row["input_length"] / input_scale)))
        output = max(1, min(max_output, round(row["output_length"] / output_scale)))
        hash_ids = row.get("hash_ids") or [row["timestamp"]]
        body = {"model": poc.BASELINE, "stream": False, "temperature": 0, "max_tokens": output}
        if text is None:
            words: list[str] = []
            for hash_id in hash_ids:
                words += poc.block_text(hash_id, block)
            words = (words + poc.block_text(row["timestamp"], target))[:target]
            shared, unique = words[:-block] or words[:1], words[-block:]
            sent["input_words"] += len(words)
            body |= {"ignore_eos": True, "messages": [{"role": "system", "content": " ".join(shared)}, {"role": "user", "content": " ".join(unique)}]}
        else:
            # Only the blocks the capped request still covers, so a capped row keeps its
            # leading prefix and drops its tail, as the word replay does.
            body["messages"], planned = prompt_corpus.compose(text, hash_ids[:max(1, -(-target // block))], target, block, index)
            sent["planned_input_tokens"] += planned
            sent["input_words"] += sum(len(message["content"].split()) for message in body["messages"])
        sent["max_tokens"] += output
        request = urllib.request.Request(f"http://127.0.0.1:{poc.API_PORT}/v1/chat/completions", data=json.dumps(body).encode(), method="POST")
        session = hash_ids[0] if row.get("hash_ids") else 0
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
        tokens = sum(pool.map(one, enumerate(rows)))
    seconds = time.monotonic() - began
    note = f" · {len(failures)} of {len(rows)} requests failed ({', '.join(sorted(set(failures)))})" if failures else ""
    log("traffic", f"done in {seconds:.0f} s: {tokens} output tokens reported by the server{note} · trace: kvcache-ai/Mooncake FAST'25 (Apache-2.0)")
    if len(failures) > len(rows) // 2:
        fail("most replayed requests failed; check the gateway and the model server before measuring")
    return {"requests": len(rows), "failed": len(failures), "seconds": round(seconds, 1), "output_tokens": tokens, "input_words_sent": sent["input_words"],
            "input_scale": round(input_scale, 4), "output_scale": round(output_scale, 4), "max_input_tokens": max_input, "max_output_tokens": max_output, "spread_seconds": spread_seconds,
            "text": {"source": "operator_corpus", "corpus": text.identity, "planned_input_tokens": sent["planned_input_tokens"], "answers": "end where the model ends them, capped at the recorded length"}
            if text else {"source": "generated_words", "answers": "forced to the recorded length (ignore_eos)"}}


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
                                               # The pinned config.json says whether the model has its own
                                               # prediction head; without it the planner skips speculation.
                                               "--diagnosis-endpoint", poc.BASELINE, "--allow-model-metadata-network", "--include-simulated", "--output", "json"))
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
    to the current setup. A setup the remote GPU cannot run as measured is skipped with
    the reason, and the copy of the current setup is added last.

    Speculative decoding is measured only when the traffic has real text. Then the
    publisher's documented speculation option is always one of the measured setups,
    whether or not the planner proposed it: it is the baseline a result has to beat, and
    the record says which of the two happened."""
    diagnosis = proposal.get("diagnosis") or {}
    log("diagnose", f"{diagnosis.get('state', 'no diagnosis')}: {diagnosis.get('summary') or diagnosis.get('primary_bottleneck') or 'the planner recorded no summary'}")
    start_id = (proposal.get("starting_point") or {}).get("candidate_id")
    start = next((c for c in proposal.get("candidates", []) if c["id"] == start_id), None)
    if start is None:
        fail(f"the planner's proposal has no starting setup: {json.dumps(proposal)[:600]}")
    start_runtime = start["deployment"]["runtime"]
    real_text = RUN["traffic"] == "real-text"
    record, runnable = [], []
    for candidate in proposal["candidates"]:
        if candidate["id"] == start_id:
            continue
        reason, deployment = candidate.get("reason") or {}, candidate["deployment"]
        runtime_doc = deployment["runtime"]
        args = models.with_change(CONTROL[2], start_runtime.get("args") or [], runtime_doc.get("args") or [])
        row = {"label": reason.get("label") or candidate.get("configuration_profile"), "technique": reason.get("technique"), "priority": reason.get("priority"),
               "rules": reason.get("rules", []), "summary": reason.get("summary", ""),
               "evidence": reason.get("evidence", []), "planner_args": runtime_doc.get("args") or [], "args": args}
        skipped = ""
        if (deployment.get("resources") or {}).get("gpu_count", 1) != 1 or (deployment.get("provider") or {}).get("adapter") != "kubernetes":
            skipped = "it needs more than one GPU or another provider adapter"
        elif (runtime_doc.get("env") or {}) != (start_runtime.get("env") or {}):
            skipped = "it changes the engine's environment, which every setup of a run shares"
        elif models.speculates(args) and not real_text:
            skipped = "speculative decoding: the replayed traffic has no real text, so draft acceptance would not be honest (run with --traffic real-text)"
        elif args == list(CONTROL[2]):
            skipped = "the current setup already makes this change"
        elif MODEL.max_num_seqs and int(dict(models.parse(args)).get("--max-num-seqs") or 0) > MODEL.max_num_seqs:
            skipped = f"vLLM cannot start it on one H100: {MODEL.id} fits at most {MODEL.max_num_seqs} requests at once"
        elif any(args == other["args"] for other in runnable):
            skipped = "a setup ranked higher already makes this change"
        else:
            try:
                models.check_args(["--served-model-name", MODEL_ID, *args], MODEL_ID)
            except ValueError as error:
                skipped = f"the remote GPU refuses it: {error}"
        row["skipped"] = skipped
        row["profile"] = candidate.get("configuration_profile") or "planner-setup"
        record.append(row)
        if skipped:
            log("plan", f"skip “{row['label']}”: {skipped}")
        else:
            runnable.append(row)
    # A smoke run measures one setup next to the current setup and its copy: with real
    # text the speculation setup, which is the path a paid campaign most needs proven.
    room = 1 if RUN["smoke"] else CANDIDATE_SLOTS
    # --planner-setups 0 records the planner's plan and measures none of it beyond the
    # speculation setup: a campaign of the current setup, that setup and the copy.
    slots = max(0 if real_text else 1, min(RUN["planner_setups"], room))
    measured = runnable[:slots]
    documented = None
    if real_text and MODEL.publisher_speculation and not models.speculates(CONTROL[2]):
        own = next((row for row in runnable if row["args"] == MODEL.speculation_args), None)
        documented = {"args": MODEL.speculation_args, "source": MODEL.speculation_source, "proposed_by_planner": own is not None,
                      "planner_rank": runnable.index(own) + 1 if own else None}
        if own is None or own not in measured:
            measured = measured[:room - 1]
            if own is not None:
                measured.append(own)
    for row in runnable:
        if row not in measured:
            row["skipped"] = ("this run measures one setup next to the current setup and its copy, and with real text that is the speculation setup" if documented and (RUN["smoke"] or slots == 0) else
                              f"only the planner's first {len(measured)} setup{'s fit' if len(measured) != 1 else ' fits'} next to the current setup and its copy")
            log("plan", f"skip “{row['label']}”: {row['skipped']}")
    chosen = [CONTROL]
    for row in measured:
        chosen.append((row["label"], row.pop("profile"), row["args"]))
        log("plan", f"try “{row['label']}”: {row['summary'] or ', '.join(row['rules'])}  [change: {' '.join(row['planner_args'])}]")
    for row in record:
        row.pop("profile", None)
    if documented and not documented["proposed_by_planner"]:
        chosen.append((DOCUMENTED_LABEL, "publisher-speculation", MODEL.speculation_args))
        log("plan", f"also measure “{DOCUMENTED_LABEL}”: the planner did not propose it, and it is the baseline a result has to beat  [change: {' '.join(MODEL.publisher_speculation)}]")
    if len(chosen) > 1:
        chosen.append(COPY)
        log("plan", f"also measure “{COPY[0]}”: it must not win, or the run proves nothing")
    CANDIDATES[:] = chosen
    save({"plan": [{"label": label, "profile": profile, "args": args} for label, profile, args in chosen],
          "planner": {"diagnosis": diagnosis, "input_diagnosis": (proposal.get("input") or {}).get("diagnosis"), "model_features": (proposal.get("input") or {}).get("model_features"),
                      "setups": record, "documented_speculation": documented}})
    return record


def proposal(digest: str) -> dict:
    # Kubernetes + this GPU + vLLM has no live qualification row, so the planner only offers it
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
        # One GPU per setup: the comparison is per GPU, and the broker runs at most four.
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


DRAFT_TOKENS, ACCEPTED_TOKENS = "vllm:spec_decode_num_draft_tokens", "vllm:spec_decode_num_accepted_tokens"


def speculation_evidence(measured: dict) -> dict | None:
    """Drafted and accepted tokens per arm of a measurement, summed over its runs, from the
    engine counters the product stored with each run (AIPerf's server-metrics export). An
    arm whose runs carry no such counters is absent: unknown, never zero. Throughput alone
    cannot tell a working drafter from one that loaded and was ignored."""
    arms: dict = {}
    for window, runs in (("first", measured.get("runs") or []), ("fresh_servers", (measured.get("confirmation") or {}).get("runs") or [])):
        for run in runs:
            metrics = (run.get("server_metrics") or {}).get("metrics") or {}
            drafted = ((metrics.get(DRAFT_TOKENS) or {}).get("stats") or {}).get("total")
            accepted = ((metrics.get(ACCEPTED_TOKENS) or {}).get("stats") or {}).get("total")
            if drafted is None or accepted is None:
                continue
            arm = arms.setdefault(f"{run.get('arm', 'candidate')}/{window}", {"draft_tokens": 0.0, "accepted_tokens": 0.0, "runs": 0})
            arm["draft_tokens"] += drafted
            arm["accepted_tokens"] += accepted
            arm["runs"] += 1
    for arm in arms.values():
        arm["acceptance"] = round(arm["accepted_tokens"] / arm["draft_tokens"], 4) if arm["draft_tokens"] else None
    return arms or None


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
    out |= {"speculation": speculation_evidence(measured), "prompt_text": (workload_doc.get("replay") or {}).get("prompt_text"),
            "output_tps": row.get("output_token_throughput"), "comparison": measured.get("comparison") or {},
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


def laptop_load() -> dict:
    """The laptop's load averages now. The control plane, the judge and the load generator
    run on it, so a busy laptop is part of a campaign's evidence."""
    one, five, fifteen = os.getloadavg()
    return {"at": dt.datetime.now(dt.timezone.utc).replace(microsecond=0).isoformat().replace("+00:00", "Z"), "load_average": [round(one, 2), round(five, 2), round(fifteen, 2)], "cpus": os.cpu_count()}


def run_settings(text: prompt_corpus.Corpus | None) -> dict:
    """What this run measures with, kept in its record and compared on --reuse-capture."""
    return {"traffic": RUN["traffic"], "shape": RUN["shape"], "requests_at_once": next(level for level, name in LOAD_PROFILES.items() if name == PROFILE), "corpus": text.identity if text else None, "smoke": RUN["smoke"], "stand_in": RUN["stand_in"],
            "current_setup": RUN["current_setup"], "engine": models.ENGINE.key, "runtime_image": VLLM_IMAGE, "gpu_type": poc.ACCELERATOR}


def run_campaign(budget: float, requests_total: int | None, reuse_capture: bool) -> None:
    if not state().get("model_image"):
        fail("run `poc_modal.py up` first")
    previous = state()
    text = None
    if RUN["traffic"] == "real-text":
        try:
            text = prompt_corpus.load(RUN["corpus"])
        except (OSError, ValueError) as error:
            fail(f"--traffic real-text needs a prompt corpus: {error}")
        corpus_in_control_plane(text)
    elif (previous.get("corpus") or {}).get("digest"):
        fail("the control plane holds a prompt corpus, so it would measure real text; run with --traffic real-text, or `poc_modal.py up` without --corpus")
    if not MODEL.setup.checked and not RUN["stand_in"]:
        log("model", f"{MODEL_ID} has no recorded serving check on {models.ENGINE.key}; run `modal_gpu.py::main --action check` before paying for a campaign")
    smoke = SMOKE if RUN["smoke"] else {}
    requests_total = requests_total or smoke.get("requests", 200)
    settings = run_settings(text)
    publisher_check()
    if reuse_capture and not (previous.get("capture_digest") and previous.get("plan") and previous.get("diagnosis")):
        fail("--reuse-capture needs an earlier run's recorded traffic shape and diagnosis")
    if reuse_capture and {key: value for key, value in (previous.get("run") or {}).items() if key != "stand_in"} != {key: value for key, value in settings.items() if key != "stand_in"}:
        fail("--reuse-capture needs the same traffic, corpus, engine, GPU type and current setup as the run that recorded the traffic")
    poc.port_forward()
    poc.reset_previous()  # clears the plan and diagnosis; a reused capture keeps its own
    kept = {key: previous.get(key) for key in ("capture_digest", "plan", "diagnosis", "traffic", "planner")} if reuse_capture else {"traffic": None, "capture_digest": None, "planner": None}
    save({"network": {}, "run_started_at": time.time(), "run_id": __import__("secrets").token_hex(8), "run": settings, "laptop_at_start": laptop_load()} | kept)
    log("laptop", f"load average {os.getloadavg()[0]:.1f} on {os.cpu_count()} CPUs at the start; a campaign wants a quiet laptop")
    # Worst case with real text: the traffic server, four campaign servers (current setup,
    # two setups to try, the copy), and two fresh servers for every setup whose gain is
    # measured again, the copy included if its first window errs: eleven. One more for a
    # server Modal preempts. The dollar cap, not this count, bounds the spend.
    broker = BrokerProcess(budget, dry=RUN["stand_in"], max_starts=12 if text else 6)
    log("broker", f"remote {server_kind()} servers capped at ${budget:.2f} for this run (estimate from Modal list prices), at most {CANDIDATE_SLOTS + 2} at once")
    try:
        check_broker_reachable()
        if reuse_capture:
            digest, diagnosis = previous["capture_digest"], previous["diagnosis"]
            CANDIDATES[:] = [(row["label"], row["profile"], row["args"]) for row in previous["plan"]]
            log("capture", f"reusing the recorded traffic shape sha256:{digest[:12]}… and its diagnosis: {', '.join(row['label'] for row in previous['plan'][1:])}")
        else:
            deploy_baseline()
            planner = Planner(requests_total)
            scales = shape_scales(RUN["shape"], poc.mooncake_rows()[:requests_total])
            traffic = replay_mooncake(requests_total, input_scale=scales["input_scale"], max_output=smoke.get("max_output_tokens", scales["max_output_tokens"]), workers=96,
                                      progress=planner.progress, max_input=smoke.get("max_input_tokens", MAX_INPUT_TOKENS), spread_seconds=smoke.get("spread_seconds"), text=text,
                                      output_scale=scales["output_scale"])
            traffic["shape"] = RUN["shape"]
            save({"traffic": traffic})
            plan(planner.finish())
            diagnosis = state().get("planner")
            digest = poc.capture_replay()
            save({"capture_digest": digest})
            log("deploy", f"removing {poc.BASELINE}: its traffic shape is recorded, and its server is not needed for the campaign")
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
        # Each share must also cover the hour the control plane reserves for every machine
        # it replaces: the fresh server a gain is measured again on (the current setup gets
        # one for every other setup that gains) and up to two machines lost to the provider.
        # Without that room the first setup that gained failed with
        # optimization_confirmation_not_covered instead of being measured again.
        replacements = len(doc["candidates"]) - 1 + 2
        approval = round(GPU_HOURLY_USD * (APPROVAL_HOURS + replacements) * len(doc["candidates"]) * 1.05, 2)
        approved = lambda: poc.campaign_record(campaign).get("state") != "awaiting_approval"  # noqa: E731
        poc.cli("optimize", "approve", campaign, "--max-cost-usd", str(approval), "--expires-in", f"{APPROVAL_HOURS}h", repeatable=False, landed=approved)
        log("approve", f"campaign approved up to ${approval} at the stated {poc.ACCELERATOR} rate (${GPU_HOURLY_USD}/h, {APPROVAL_HOURS} h window); real spend stays under the broker cap")
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
                     "speculation": metrics.get("speculation"), "prompt_text_corpus": ((metrics.get("prompt_text") or {}).get("corpus") or {}).get("digest"),
                     "output_tokens_per_second": tps, "request_throughput": metrics.get("request_throughput"), "ttft_p95_ms": metrics.get("ttft_p95_ms"),
                     "latency_p95_ms": metrics.get("latency_p95_ms"), "tpot_p95_ms": metrics.get("tpot_p95_ms"),
                     "usd_per_million_output_tokens": (GPU_HOURLY_USD / (tps * 3600) * 1_000_000) if tps and not RUN["stand_in"] else None,
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
    settings, warnings, text_check = current.get("run") or run_settings(None), [], None
    plan_args = {row["label"]: row["args"] for row in current.get("plan") or []}
    measured_rows = [row for row in rows if row["output_tokens_per_second"] is not None]
    if settings.get("corpus"):
        # The campaign must have composed its requests from this run's corpus; a control
        # plane that fell back to synthetic text measured another workload.
        seen = sorted({row["prompt_text_corpus"] or "synthetic text" for row in measured_rows})
        text_check = {"expected": settings["corpus"]["digest"], "measured_on": seen, "ok": seen in ([], [settings["corpus"]["digest"]])}
        if not text_check["ok"]:
            decision = "no decision: the campaign did not measure on this run's prompt corpus"
    for row in measured_rows:
        if models.speculates(plan_args.get(row["label"]) or []):
            own = [arm for name, arm in (row["speculation"] or {}).items() if name.startswith("candidate/")]
            if not own:
                warnings.append(f"“{row['label']}” speculates but its runs carry no acceptance counters: a working drafter cannot be told from one that loaded and was ignored")
            elif not any(arm["accepted_tokens"] for arm in own):
                warnings.append(f"“{row['label']}” drafted tokens and none was accepted")
    ledger = broker.ledger()
    for lease in ledger.get("leases", []):
        if lease.get("reason") == "preempted":
            warnings.append(f"Modal preempted the server behind pod {lease.get('pod')} during the run; its lease ended and the pod started a new server on another machine")
    if settings.get("stand_in"):
        decision = f"stand-in servers, no model and no GPU: “{decision}” is the pipeline's answer to scripted speeds, not a measurement"
    summary = {"schema_version": "infercrane.poc-modal-result/v3", "campaign_id": campaign, "campaign_state": record.get("state"), "decision": decision,
               "run": settings, "prompt_text_check": text_check, "warnings": warnings,
               "copy_of_current_setup": {"must_not_win": True, "won": copy_won, "state": copy and copy["state"], "failure_code": copy and copy["failure_code"],
                                         "objective_decision": copy and objective_decision(copy)} if copy else None,
               "model": {"id": MODEL_ID, "revision": MODEL_REVISION, "chosen_by": "poc_modal.py choose-model: most downloaded FP8 checkpoint from Qwen, Z.ai and DeepSeek that fits one H100"},
               "current_setup": {"args": CONTROL[2], "env": MODEL.env, "publisher_args": list(MODEL.publisher_args), "publisher_env": MODEL.publisher_env,
                                 "added_args": list(MODEL.added_args), "added_because": MODEL.added_because, "source": MODEL.source}, "planner": current.get("planner"),
               "runtime_image": VLLM_IMAGE, "engine": models.ENGINE.key,
               "gpu": "CPU stand-in servers on Modal (no GPU, no model)" if settings.get("stand_in") else f"1 x {models.ACCELERATOR.description} on Modal (gpu=\"{models.ACCELERATOR.modal_gpu}\")",
               "rate": {"usd_per_hour": GPU_HOURLY_USD, "basis": f"Modal list price, {models.ACCELERATOR.description}, GPU only, read {models.ACCELERATOR.price_read}"},
               "load": {"profile": PROFILE, "requests_at_once": settings.get("requests_at_once"), "objective": OBJECTIVE, "trace": "Mooncake FAST'25 arxiv trace, first rows, lengths scaled to the run's shape and capped",
                        "shape": settings.get("shape"), "shape_medians": SHAPES.get(settings.get("shape") or "mooncake"),
                        "trace_license": "Apache-2.0 (kvcache-ai/Mooncake)", "traffic": current.get("traffic"), "reasoning": REASONING_NOTE},
               "measured_from": "the control plane in a kind cluster on the laptop; every request crosses the internet to Modal through a tunnel",
               "laptop": {"at_start": current.get("laptop_at_start"), "at_end": laptop_load()},
               "diagnosis": current.get("diagnosis"), "setups": rows, "network": current.get("network"), "broker": ledger}
    (OUTPUT / f"{campaign}-result.json").write_text(json.dumps(summary, indent=2))
    text_words = f"real text from “{settings['corpus']['name']}”" if settings.get("corpus") else "generated words"
    lines = [f"Traffic: Mooncake (Kimi) production trace replay, shape “{settings.get('shape') or 'mooncake'}”, {text_words} · model {MODEL_ID} · vLLM {models.ENGINE.version} · one {server_kind()} per setup on Modal", ""]
    if settings.get("stand_in"):
        lines += ["STAND-IN SERVERS: no model and no GPU ran. Every speed below is scripted; this run proves the path only.", ""]
    if copy:
        lines += [("The copy of the current setup measured better: this run proves nothing." if copy_won else
                   "The copy of the current setup did not win, as it must not."), ""]
    for row in rows:
        tps, cost = row["output_tokens_per_second"], row["usd_per_million_output_tokens"]
        match = "-" if row["answer_match"] is None else f"{row['answer_match']:.0%}"
        lines.append(row["label"])
        lines.append(f"   {('%.0f' % tps) if tps else '-'} output tok/s · p95 latency {('%.0f ms' % row['latency_p95_ms']) if row['latency_p95_ms'] else '-'} · "
                     f"p95 TTFT {('%.0f ms' % row['ttft_p95_ms']) if row['ttft_p95_ms'] else '-'} · ${('%.3f' % cost) if cost else '-'} per 1M output tokens · answers {match}")
        for name, arm in (row["speculation"] or {}).items():
            if name.startswith("candidate/") and arm["acceptance"] is not None:
                lines.append(f"   speculation ({name.split('/', 1)[1].replace('_', ' ')} window): {arm['acceptance']:.0%} of {arm['draft_tokens']:.0f} drafted tokens accepted")
        if row is not rows[0]:
            lines.append(f"   {poc.words(row['state'] or '', row['failure_code'])}")
    lines += [f"Warning: {warning}" for warning in warnings]
    lines += ["", f"Decision: {summary['decision']}. Nothing is activated (the run stops at the proposal).",
              f"Costs use Modal's {poc.ACCELERATOR} list price (${GPU_HOURLY_USD}/h, GPU only). Latency includes the laptop-to-Modal network path.",
              "Tokens per second count reasoning tokens; requests leave the model's reasoning on. Answers: word overlap on 24 short prompts.",
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
    parser.add_argument("--requests", type=int, help="Mooncake rows to replay through the gateway (default 200, or 40 with --smoke)")
    parser.add_argument("--reuse-capture", action="store_true", help="skip the baseline and replay; campaign on the traffic shape the last run recorded")
    parser.add_argument("--engine", choices=sorted(models.ENGINES), default=models.ENGINE.key, help="the vLLM image every setup of the run serves with, pinned by digest")
    parser.add_argument("--gpu", choices=sorted(models.ACCELERATORS), default=models.ACCELERATOR.key, help="the GPU type every setup of the run serves on")
    parser.add_argument("--traffic", choices=TRAFFIC, default="mooncake-words", help="real-text composes every request from --corpus, so speculative setups are measured")
    parser.add_argument("--shape", choices=sorted(SHAPES), default="mooncake", help="scale the trace's lengths to a traffic shape: the trace as it is, or the medians of the product's public interactive, long-prefill and decode-heavy shapes")
    parser.add_argument("--requests-at-once", type=int, choices=sorted(LOAD_PROFILES), default=32, help="the load every measured run of the campaign holds (its benchmark profile's concurrency)")
    parser.add_argument("--corpus", type=Path, help=f"prompt corpus for `up` and for `run --traffic real-text` (default for both: the control plane's, else {prompt_corpus.STAND_IN.name}, a rehearsal corpus too small for a measurement)")
    parser.add_argument("--current-setup", choices=CURRENT_SETUPS, default="publisher", help="the setup every other setup must beat: the publisher's command, or that command with its documented speculation option")
    parser.add_argument("--planner-setups", type=int, choices=(0, 1, 2), help="how many of the planner's setups to measure (default 1, or 2 with real text); 0, with real text only, records its plan and measures the speculation setup alone")
    parser.add_argument("--smoke", action="store_true", help="shrink the traffic so the whole path costs a few dollars; the judge keeps its rounds")
    parser.add_argument("--stand-in", action="store_true", help="CPU stand-in servers instead of GPUs: the same path for cents, and no measurement")
    args = parser.parse_args()
    os.chdir(ROOT)
    STATE_DIR.mkdir(parents=True, exist_ok=True)
    configure(args.engine, args.gpu, args.current_setup)
    global PROFILE
    PROFILE = LOAD_PROFILES[args.requests_at_once]
    corpus = args.corpus.resolve() if args.corpus else None
    if args.command == "up":
        up(corpus)
        return 0
    held = (state().get("corpus") or {}).get("digest")
    RUN.update(traffic=args.traffic, shape=args.shape, smoke=args.smoke, stand_in=args.stand_in,
               planner_setups=args.planner_setups if args.planner_setups is not None else (2 if args.traffic == "real-text" else 1),
               corpus=corpus or (prompt_corpus.STAND_IN if args.traffic == "real-text" else None))
    if args.command == "run" and args.traffic == "real-text" and corpus is None and held and held != prompt_corpus.load(prompt_corpus.STAND_IN).digest:
        fail("the control plane holds another prompt corpus than the rehearsal one; name it with --corpus")
    {"choose-model": choose_model, "download": download, "dry": dry, "stop-gpus": stop_gpus, "down": down}.get(args.command, lambda: run_campaign(args.budget_usd, args.requests, args.reuse_capture))()
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
