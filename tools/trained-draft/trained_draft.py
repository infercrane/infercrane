"""Trained-draft ladder on Modal: train an EAGLE-3 draft on chat prompts, then measure it
on one GPU against the model-free drafting methods, a published draft, and no speculation.

    python3 trained_draft.py prepare  --run-id ID            # $0: oasst1 prompts, pinned and split
    python3 trained_draft.py dry-run  --run-id ID            # $0: every stage here with stand-ins
    TRAINED_DRAFT_PLAN=l4-qwen3-1.7b modal run trained_draft.py::launch --stage smoke --run-id ID --budget-usd CAP
    python3 trained_draft.py fetch    --run-id ID            # $0: records from the volume
    go run ./tools/trained-draft/judge .infercrane/trained-draft/ID   # the campaign judge's verdict

The plan (draftlib.PLANS, chosen by TRAINED_DRAFT_PLAN) pins the model, the published draft,
the GPU, the sizes and the money; its rules file holds the pass bar. Stages:

  smoke    a few answers, a few training steps, both drafts served briefly: every command
           proved before money is spent on the rest.
  train    the model answers the training prompts at temperature 0 (thinking off); the
           published draft is fine-tuned on those answers with speculators, hidden states
           from a vLLM server in the same container.
  measure  each window's servers run at once on the one GPU; the window interleaves its
           control with its candidates as a campaign does, at the plan's loads, on held-out
           prompts; answers at temperature 0 are compared; the task set when the plan has one.

The load generator runs inside the container. Prompts and answers stay on the run's Modal
volume (public data here; in a customer's account they would stay there). Only numbers
leave in the records.
"""

from __future__ import annotations

import argparse
import asyncio
import hashlib
import json
import os
import random
import shutil
import signal
import socket
import subprocess
import sys
import time
import urllib.request
from datetime import datetime, timezone
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
import draftlib as lib  # noqa: E402

HERE = Path(__file__).resolve().parent
REPOSITORY = HERE.parent.parent
LOCAL_RUNS = REPOSITORY / ".infercrane" / "trained-draft"
# The task set is not part of this repository. TRAINED_DRAFT_TASKS names its file; without it the
# harness looks in .infercrane/corpus/ of this checkout, which git ignores.
TASKS_VARIABLE = "TRAINED_DRAFT_TASKS"
QUALITY_FILE = Path(os.environ.get(TASKS_VARIABLE, str(REPOSITORY / ".infercrane" / "corpus" / "fast-track-quality-v1.json")))
QUALITY_SHA256 = "83c3df7b82f8d8a3b9fa892f1fdcd81679f93646337c8b78d456a828101d72e4"


def task_set_file() -> Path:
    """The task set's file, or a stop that says what to set."""
    if not QUALITY_FILE.is_file():
        raise SystemExit(f"no task set at {QUALITY_FILE}: set {TASKS_VARIABLE} to the task-set file (sha256 {QUALITY_SHA256})")
    return QUALITY_FILE

VOLUME_NAME = "infercrane-trained-draft"
VOLUME_ROOT = Path("/vol")
APP_NAME = "infercrane-trained-draft"
PLAN = lib.plan_for()

# Rung 1 (acceptance offline, rung1_offline.py) runs on the rung-2 plan's L4 when the laptop
# may not load models (one heavy local job at a time across sessions since 26 Nov 05:58).
RUNG1_SECONDS = 30 * 60
# The diagnose stage: four drafts served in eager mode, about a minute to start each.
DIAGNOSE_SECONDS = 20 * 60

# Each stage stops its optional work this long before its hard cap, so it always writes
# its record and commits the volume before Modal's timeout ends the container.
DEADLINE_MARGIN_SECONDS = 4 * 60
# The CPU self-test's Modal timeout (launch --stage selftest).
SELFTEST_SECONDS = 120
RUNS_PER_CANDIDATE = 4
TASK_CONCURRENCY = 24


# ---------------------------------------------------------------------------
# Prompts.
# ---------------------------------------------------------------------------


def download_oasst(directory: Path) -> dict[str, Path]:
    import hashlib

    directory.mkdir(parents=True, exist_ok=True)
    out = {}
    for split, (name, sha256) in lib.OASST_FILES.items():
        target = directory / Path(name).name
        if not target.exists() or hashlib.sha256(target.read_bytes()).hexdigest() != sha256:
            url = f"https://huggingface.co/datasets/{lib.OASST_DATASET}/resolve/{lib.OASST_REVISION}/{name}"
            urllib.request.urlretrieve(url, target)  # noqa: S310 - pinned public dataset
        actual = hashlib.sha256(target.read_bytes()).hexdigest()
        if actual != sha256:
            raise SystemExit(f"{name}: sha256 {actual} is not the pinned {sha256}")
        out[split] = target
    return out


def prepare(run_dir: Path) -> dict:
    """oasst1's train and validation splits as prompt files, with a manifest of hashes."""
    import pyarrow.parquet as pq  # local only; the container receives the JSON files

    files = download_oasst(LOCAL_RUNS / "oasst1")
    rows = {split: pq.read_table(path).to_pylist() for split, path in files.items()}
    split = lib.split_prompts(rows["train"], rows["validation"])
    prompts = run_dir / "prompts"
    for name, items in split.items():
        lib.write_json(prompts / f"{name}.json", items)
    manifest = {
        "dataset": lib.OASST_DATASET, "revision": lib.OASST_REVISION, "licence": "Apache-2.0", "files": {k: v[1] for k, v in lib.OASST_FILES.items()},
        "train": {"prompts": len(split["train"]), "fingerprint": lib.digest_lines(split["train"]), "languages": lib.languages(split["train"])},
        "held_out": {"prompts": len(split["held_out"]), "fingerprint": lib.digest_lines(split["held_out"]), "languages": lib.languages(split["held_out"])},
    }
    lib.write_json(prompts / "manifest.json", manifest)
    return manifest


# ---------------------------------------------------------------------------
# Servers.
# ---------------------------------------------------------------------------


class Server:
    """One OpenAI-compatible server process: vLLM on the GPU, or the stand-in."""

    def __init__(self, name: str, command: list[str], port: int, log_dir: Path, env: dict | None = None):
        self.name, self.port, self.url = name, port, f"http://127.0.0.1:{port}"
        log_dir.mkdir(parents=True, exist_ok=True)
        self.log_path = log_dir / f"{name}.log"
        self.log = self.log_path.open("w")
        self.process = subprocess.Popen(command, stdout=self.log, stderr=subprocess.STDOUT, env={**os.environ, **(env or {})}, start_new_session=True)  # noqa: S603

    def wait_ready(self, timeout: float) -> float:
        started = time.time()
        while time.time() - started < timeout:
            if self.process.poll() is not None:
                raise RuntimeError(f"{self.name} exited with {self.process.returncode}:\n{tail(self.log_path)}")
            try:
                with urllib.request.urlopen(self.url + "/health", timeout=2) as response:  # noqa: S310 - local
                    if response.status == 200:
                        return time.time() - started
            except OSError:
                pass
            time.sleep(2)
        raise RuntimeError(f"{self.name} not ready after {timeout:.0f} s:\n{tail(self.log_path)}")

    def metrics(self) -> str:
        with urllib.request.urlopen(self.url + "/metrics", timeout=10) as response:  # noqa: S310 - local
            return response.read().decode()

    def _post(self, path: str, timeout: float = 300) -> None:
        request = urllib.request.Request(self.url + path, data=b"", method="POST")
        with urllib.request.urlopen(request, timeout=timeout) as response:  # noqa: S310 - local
            if response.status != 200:
                raise RuntimeError(f"{self.name} {path}: HTTP {response.status}")

    def sleep(self) -> float:
        """vLLM sleep level 1: weights to CPU memory, cache discarded; the GPU is free for
        another server of the window. Needs VLLM_SERVER_DEV_MODE=1 and --enable-sleep-mode."""
        started = time.time()
        self._post("/sleep?level=1")
        return time.time() - started

    def wake(self) -> float:
        started = time.time()
        self._post("/wake_up")
        return time.time() - started

    def stop(self) -> None:
        if self.process.poll() is None:
            os.killpg(self.process.pid, signal.SIGTERM)
            try:
                self.process.wait(timeout=60)
            except subprocess.TimeoutExpired:
                os.killpg(self.process.pid, signal.SIGKILL)
                self.process.wait(timeout=30)
        self.log.close()


def free_port() -> int:
    import socket

    with socket.socket() as handle:
        handle.bind(("127.0.0.1", 0))
        return handle.getsockname()[1]


def tail(path: Path, lines: int = 30) -> str:
    try:
        return "\n".join(path.read_text(errors="replace").splitlines()[-lines:])
    except OSError:
        return ""


class Context:
    """Where a stage runs: dry (this machine, stand-ins, speculators' own --dry-run)
    or real (the Modal container with vLLM on the plan's GPU)."""

    def __init__(self, run_dir: Path, dry: bool, weights: Path, speculators: str, deadline: float | None = None, plan: lib.Plan = PLAN, dry_online: bool = False, commit=None,
                 harness: dict | None = None):
        self.run_dir, self.dry, self.weights, self.speculators = run_dir, dry, weights, speculators
        # The harness commit the launcher ran from, and whether tools/trained-draft differed
        # from it; a dirty or unknown commit leaves the contract record's commit unknown.
        self.harness = harness or {"commit": "", "dirty": True}
        # commit persists what the stage wrote so far (the Modal volume's commit), so a stage cut
        # by its timeout still leaves its progress and a partial record.
        self.commit = commit
        self.stage = ""
        # dry_online trains for real on this machine against a transformers stand-in of the
        # extraction server (standin_extract.py), with the GPU run's exact trainer flags.
        self.dry_online = dry and dry_online
        self.logs = run_dir / "logs"
        self.deadline = deadline
        self.plan = plan

    def time_left(self) -> float:
        return float("inf") if self.deadline is None else self.deadline - time.time()

    def progress(self, phase: str, record: dict | None = None, **facts) -> None:
        """One line per phase: printed (Modal streams it to the launcher's log), appended to
        {stage}-progress.jsonl on the volume, with the record so far as a partial {stage}.json,
        then committed."""
        line = {"at": now(), "stage": self.stage, "phase": phase, **facts}
        print(json.dumps(line, default=str), flush=True)
        self.run_dir.mkdir(parents=True, exist_ok=True)
        with (self.run_dir / f"{self.stage}-progress.jsonl").open("a") as handle:
            handle.write(json.dumps(line, default=str) + "\n")
        if record is not None:
            lib.write_json(self.run_dir / f"{self.stage}.json", {**record, "partial": True, "last_phase": phase})
        if self.commit is not None:
            try:
                self.commit()
            except Exception as error:  # noqa: BLE001 - evidence is best effort, the stage goes on
                print(json.dumps({"stage": self.stage, "phase": "commit-failed", "error": str(error)[:200]}), flush=True)

    @property
    def model_dir(self) -> str:
        return self.plan.model if self.dry else str(self.weights / weights_name(self.plan.model))

    @property
    def published_dir(self) -> str:
        return self.plan.published if self.dry else str(self.weights / weights_name(self.plan.published))

    def server(self, name: str, port: int, role: str, extra: list[str] | None = None, behaviour: dict | None = None) -> Server:
        if self.dry:
            # This machine runs other servers; a stand-in takes a free port so a readiness
            # probe can never reach someone else's server.
            port = free_port()
            if role == "extract":
                command = [sys.executable, str(HERE / "standin_extract.py"), "--port", str(port), "--model", self.model_dir, "--served", self.model_dir,
                           "--layers", ",".join(str(x) for x in self.plan.target_layer_ids), "--storage", str(self.run_dir / "hidden-states-online")]
                return Server(name, command, port, self.logs)
            command = [sys.executable, str(HERE / "standin.py"), "--port", str(port), "--model", self.plan.model, "--behaviour", json.dumps(behaviour or {})]
            return Server(name, command, port, self.logs)
        command, env = self.server_command(port, role, extra)
        return Server(name, command, port, self.logs, env=env)

    def server_command(self, port: int, role: str, extra: list[str] | None = None) -> tuple[list[str], dict]:
        """The vLLM command for a role. The extraction server answers under the verifier's own
        path: speculators passes that path as the model name and refuses a server that answers
        under another (speculators 0.9.0, train/data.py _setup_client)."""
        served = self.model_dir if role == "extract" else self.plan.model
        command = ["vllm", "serve", self.model_dir, "--served-model-name", served, "--host", "127.0.0.1", "--port", str(port),
                   "--gpu-memory-utilization", str(self.plan.memory_share[role]), "--max-model-len", str(self.plan.max_model_len[role]), *self.plan.model_args, *(extra or [])]
        env = {"VLLM_NO_USAGE_STATS": "1", "DO_NOT_TRACK": "1"}
        if role == "measure" and self.plan.sleep_mode:
            command.append("--enable-sleep-mode")
            env["VLLM_SERVER_DEV_MODE"] = "1"
        return command, env


def weights_name(repository: str) -> str:
    return repository.replace("/", "--").lower()


def extraction_args(storage: Path, plan: lib.Plan) -> list[str]:
    """speculators v0.9.0 scripts/launch_vllm.py's train command, without its render tuning."""
    return [
        "--speculative_config", json.dumps({"method": "extract_hidden_states", "num_speculative_tokens": 1, "draft_model_config": {"hf_config": {"eagle_aux_hidden_state_layer_ids": plan.target_layer_ids}}}),
        "--kv_transfer_config", json.dumps({"kv_connector": "ExampleHiddenStatesConnector", "kv_role": "kv_producer", "kv_connector_extra_config": {"shared_storage_path": str(storage)}}),
    ]


# ---------------------------------------------------------------------------
# Load.
# ---------------------------------------------------------------------------


async def _one(session, url: str, body: dict) -> dict:
    started = time.perf_counter()
    first, tokens, text = None, 0, []
    try:
        async with session.post(url + "/v1/chat/completions", json=body) as response:
            if response.status != 200:
                return {"ok": False, "status": response.status}
            if not body.get("stream"):
                data = await response.json()
                choice = data["choices"][0]
                return {"ok": True, "latency": time.perf_counter() - started, "data": data, "text": choice["message"].get("content") or "", "output_tokens": (data.get("usage") or {}).get("completion_tokens", 0)}
            async for raw in response.content:
                line = raw.decode().strip()
                if not line.startswith("data:") or line == "data: [DONE]":
                    continue
                chunk = json.loads(line[5:])
                for choice in chunk.get("choices") or []:
                    piece = (choice.get("delta") or {}).get("content")
                    if piece:
                        if first is None:
                            first = time.perf_counter() - started
                        text.append(piece)
                if chunk.get("usage"):
                    tokens = chunk["usage"].get("completion_tokens", 0)
    except Exception as error:  # noqa: BLE001 - a failed request is recorded, never raised
        return {"ok": False, "error": type(error).__name__}
    return {"ok": first is not None and tokens > 0, "ttft": first, "latency": time.perf_counter() - started, "output_tokens": tokens, "text": "".join(text)}


async def _load(url: str, bodies: list[dict], concurrency: int) -> tuple[list[dict], float]:
    import aiohttp

    results: list[dict] = [{} for _ in bodies]
    queue: asyncio.Queue = asyncio.Queue()
    for index in range(len(bodies)):
        queue.put_nowait(index)
    timeout = aiohttp.ClientTimeout(total=1800)
    async with aiohttp.ClientSession(timeout=timeout, connector=aiohttp.TCPConnector(limit=concurrency)) as session:
        async def worker():
            while not queue.empty():
                index = queue.get_nowait()
                results[index] = await _one(session, url, bodies[index])

        started = time.perf_counter()
        await asyncio.gather(*(worker() for _ in range(min(concurrency, len(bodies)))))
        return results, time.perf_counter() - started


def load(url: str, bodies: list[dict], concurrency: int) -> tuple[list[dict], float]:
    """Closed-loop load: concurrency requests in flight until every body is sent."""
    return asyncio.run(_load(url, bodies, concurrency))


# ---------------------------------------------------------------------------
# Stages.
# ---------------------------------------------------------------------------


def read_prompts(ctx: Context, name: str) -> list[dict]:
    return json.loads((ctx.run_dir / "prompts" / f"{name}.json").read_text())


def regenerate(ctx: Context, prompts: list[dict], out: Path) -> dict:
    """The model answers every prompt at temperature 0; each answer becomes one
    training row of token ids (prompt masked out). Only token ids are written."""
    plan = ctx.plan
    server = ctx.server("generate", 8000, "generate", ["--max-num-seqs", str(plan.generation_concurrency)], {"tokens_per_second": 4000, "answer_tokens": 48})
    try:
        ready = server.wait_ready(900)
        bodies = [lib.chat_request(plan.model, p["content"], plan.generation_max_tokens, stream=False, token_ids=True) for p in prompts]
        # macOS caps a listen queue at 128, so the dry run asks less at once than the GPU run.
        results, seconds = load(server.url, bodies, plan.generation_concurrency if not ctx.dry else 64)
        failed = [index for index, result in enumerate(results) if not result.get("ok")]
        if failed:
            # One retry pass for transient failures; what fails twice is left out and counted.
            again, more = load(server.url, [bodies[index] for index in failed], 32)
            for index, result in zip(failed, again):
                results[index] = result
            seconds += more
    finally:
        server.stop()
    rows = [lib.training_row(p["id"], r.get("data") or {}) for p, r in zip(prompts, results)]
    kept = [row for row in rows if row]
    out.parent.mkdir(parents=True, exist_ok=True)
    with out.open("w") as handle:
        for row in kept:
            handle.write(json.dumps({"input_ids": row["input_ids"], "loss_mask": row["loss_mask"]}) + "\n")
    answer_tokens = sum(sum(row["loss_mask"]) for row in kept)
    return {"prompts": len(prompts), "rows": len(kept), "failed": len(prompts) - len(kept), "retried": len(failed), "answer_tokens": answer_tokens,
            "tokens": sum(len(row["input_ids"]) for row in kept), "ready_seconds": round(ready, 1), "seconds": round(seconds, 1),
            "fingerprint": lib.digest_lines([{"id": row["id"], "input_ids": row["input_ids"]} for row in kept])}


def run_speculators(ctx: Context, args: list[str], log_name: str, timeout: float) -> tuple[int, float]:
    log = ctx.logs / f"{log_name}.log"
    log.parent.mkdir(parents=True, exist_ok=True)
    started = time.time()
    with log.open("w") as handle:
        process = subprocess.Popen([ctx.speculators, *args], stdout=handle, stderr=subprocess.STDOUT, env={**os.environ, "HF_HUB_DISABLE_TELEMETRY": "1", "COLUMNS": "100000"}, start_new_session=True)  # noqa: S603
        try:
            code = process.wait(timeout=timeout)
        except subprocess.TimeoutExpired:
            os.killpg(process.pid, signal.SIGTERM)
            try:
                process.wait(timeout=120)
            except subprocess.TimeoutExpired:
                os.killpg(process.pid, signal.SIGKILL)
            code = -1
    return code, time.time() - started


def newest_checkpoint(save_path: Path) -> Path | None:
    candidates = [d for d in save_path.iterdir() if d.is_dir() and not d.is_symlink() and (d / "config.json").exists() and any(d.glob("*.safetensors"))] if save_path.exists() else []
    if (save_path / "config.json").exists() and any(save_path.glob("*.safetensors")):
        candidates.append(save_path)
    return max(candidates, key=lambda d: (d / "config.json").stat().st_mtime) if candidates else None


def copy_draft(checkpoint: Path, draft_dir: Path) -> None:
    """Only what a server needs: the config and the weights, never optimizer state."""
    if draft_dir.exists():
        shutil.rmtree(draft_dir)
    draft_dir.mkdir(parents=True)
    for path in checkpoint.iterdir():
        if path.is_file() and (path.suffix in (".json", ".safetensors", ".py")) and "optimizer" not in path.name and "scheduler" not in path.name:
            shutil.copy2(path, draft_dir / path.name)


def train_draft(ctx: Context, rows_path: Path, max_steps: int | None, loop_seconds: float) -> dict:
    """speculators prepare-data on the token rows, then speculators train from the
    published draft with hidden states generated online by a vLLM extraction server."""
    prepared = Path("/tmp/trained-draft-prepared") if not ctx.dry else ctx.run_dir / "prepared"
    code, seconds = run_speculators(ctx, ["prepare-data", "--model", ctx.model_dir, "--data", str(rows_path), "--output", str(prepared), "--seq-length", str(ctx.plan.total_seq_len), "--minimum-valid-tokens", "1", "--overwrite"], "prepare-data", 1800)
    if code != 0:
        raise RuntimeError(f"prepare-data exited {code}:\n{tail(ctx.logs / 'prepare-data.log')}")
    save_path = (ctx.run_dir if ctx.dry else Path("/tmp")) / "checkpoints"
    facts: dict = {"prepare_seconds": round(seconds, 1)}
    start_from = ctx.published_dir
    if ctx.plan.convert_published:
        # The published draft is in the original EAGLE-3 format; speculators starts from its own.
        start_from = str((ctx.run_dir if ctx.dry else Path("/tmp")) / "published-speculators")
        if not (Path(start_from) / "config.json").exists():
            code, seconds = run_speculators(ctx, ["convert", ctx.published_dir, "--algorithm", "eagle3", "--verifier", ctx.model_dir, "--output-path", start_from], "convert", 1200)
            if code != 0:
                raise RuntimeError(f"convert exited {code}:\n{tail(ctx.logs / 'convert.log')}")
            facts["convert_seconds"] = round(seconds, 1)
        # A converted draft has no token embedding, and speculators 0.9.0 would train it with zeros
        # in its place (draft_embedding.py); the verifier's embedding goes into the checkpoint.
        facts["embedding"] = add_verifier_embedding(ctx, start_from)
    common = ["train", "--speculator-type", "eagle3", "--from-pretrained", start_from, "--verifier-name-or-path", ctx.model_dir, "--data-path", str(prepared),
              "--save-path", str(save_path), "--seed", str(lib.SEED)]
    if ctx.dry and not ctx.dry_online:
        # No GPU: speculators' own dry run builds the draft from the published one,
        # loads the verifier's weights it needs, and saves a checkpoint.
        code, seconds = run_speculators(ctx, [*common, "--dry-run"], "train", 3600)
        facts.update({"train_exit": code, "train_seconds": round(seconds, 1), "mode": "speculators --dry-run (no training step on CPU)"})
    else:
        storage = Path("/dev/shm/hidden_states") if not ctx.dry else ctx.run_dir / "hidden-states-online"
        if ctx.dry:
            facts["mode"] = "real training steps on this machine against the transformers stand-in of the extraction server"
        storage.mkdir(parents=True, exist_ok=True)
        extractor = ctx.server("extract", 8001, "extract", extraction_args(storage, ctx.plan))
        try:
            facts["extract_ready_seconds"] = round(extractor.wait_ready(900 if not ctx.dry else 600), 1)
            ctx.progress("extractor-ready", seconds=facts["extract_ready_seconds"])
            args = [*common, "--hidden-states-path", str(storage), "--vllm-endpoint", extractor.url + "/v1", "--on-missing", "generate", "--total-seq-len", str(ctx.plan.total_seq_len),
                    "--epochs", str(ctx.plan.epochs), "--checkpoint-freq", "1", "--lr", "1e-4", "--scheduler-type", "cosine", "--scheduler-warmup-ratio", "0.05", "--num-workers", "4", "--log-freq", "20"]
            if max_steps:
                args += ["--max-steps", str(max_steps), "--epochs", "1"]
            if ctx.dry:
                # No CUDA here: FlexAttention has no backward on Apple GPUs and fused losses need
                # Triton. The GPU run keeps speculators' defaults.
                args += ["--draft-attn-impl", "sdpa", "--loss-implementation", "eager"]
            code, seconds = run_speculators(ctx, args, "train", loop_seconds)
            if code != 0 and "flex" in tail(ctx.logs / "train.log", 80).lower():
                # The fused attention needs a compiler the image may lack; sdpa trains the same layer.
                code, seconds2 = run_speculators(ctx, [*args, "--draft-attn-impl", "sdpa"], "train-sdpa", loop_seconds)
                seconds += seconds2
                facts["attention"] = "sdpa"
            facts.update({"train_exit": code, "train_seconds": round(seconds, 1), "stopped_at_wall_clock": code == -1})
        finally:
            extractor.stop()
    checkpoint = newest_checkpoint(save_path)
    if checkpoint is None:
        raise RuntimeError(f"training left no checkpoint (exit {facts.get('train_exit')}):\n{tail(ctx.logs / 'train.log', 60)}")
    copy_draft(checkpoint, ctx.run_dir / "draft")
    facts["checkpoint"] = checkpoint.name
    return facts


def add_verifier_embedding(ctx: Context, draft_dir: str) -> dict:
    """draft_embedding.py with the speculators environment's Python (it has torch)."""
    python = Path(shutil.which(ctx.speculators) or ctx.speculators).with_name("python")
    done = subprocess.run([str(python), str(HERE / "draft_embedding.py"), draft_dir, ctx.model_dir], capture_output=True, text=True, check=False, timeout=1800)  # noqa: S603
    if done.returncode != 0:
        raise RuntimeError(f"draft_embedding.py exited {done.returncode}:\n{done.stderr[-2000:]}")
    return json.loads(done.stdout.strip().splitlines()[-1])


def stage_smoke(ctx: Context) -> dict:
    """Every command of the real run on a few prompts: answer, train, serve both drafts."""
    record: dict = {"stage": "smoke", "started_at": now()}
    train = read_prompts(ctx, "train")[:64]
    held_out = read_prompts(ctx, "held_out")[:8]
    rows = ctx.run_dir / "smoke" / "rows.jsonl"
    if ctx.plan.adopt_draft:
        # The draft comes from another run (adopt); the smoke checks it and serves it.
        record["adopted"] = adopted_draft(ctx)
        record["regenerate"] = {"rows": 0, "adopted": True}
    else:
        record["regenerate"] = regenerate(ctx, train, rows)
        ctx.progress("answers", record, rows=record["regenerate"]["rows"], seconds=record["regenerate"]["seconds"])
        record["train"] = train_draft(ctx, rows, max_steps=10, loop_seconds=6 * 60)
        ctx.progress("trained", record, exit=record["train"].get("train_exit"), train_seconds=record["train"].get("train_seconds"))
    record["serve"] = {}
    # The arm that proves this commit's training serves first, so a stage cut at its deadline
    # loses a model-free arm, never the trained draft.
    planned = {arm for window in ctx.plan.windows for arm in (window.control, *window.candidates)} - {"none"}
    # A server without speculation (none, none_b) has no acceptance to read and is the control's own
    # command, which the measure stage serves; the smoke serves the drafting arms.
    arms = [arm for arm in ("trained", "published", "suffix", "ngram") if arm in planned]
    # Eager mode skips torch.compile and CUDA graph capture (2 to 3 minutes a start on an L4), which
    # the smoke check does not need; a sleep-mode plan keeps them, since that is what it proves.
    eager = [] if ctx.plan.sleep_mode else ["--enforce-eager"]
    record["skipped"] = []
    for offset, name in enumerate(arms):
        if ctx.time_left() < 4 * 60:
            record["skipped"].append(f"{name}: past the stage deadline")
            continue
        server = ctx.server(name, 8101 + offset, "measure", [*eager, *ctx.plan.arm_args(name, str(ctx.run_dir / "draft"))], {"accept": 0.6})
        try:
            ready = server.wait_ready(900)
            cycle = None
            if ctx.plan.sleep_mode:
                # The measure stage alternates arms through sleep mode; prove it here first.
                cycle = {"sleep_seconds": round(server.sleep(), 2), "wake_seconds": round(server.wake(), 2)}
            before = lib.speculation_counters(server.metrics())
            results, seconds = load(server.url, [lib.chat_request(ctx.plan.model, p["content"], 128, stream=True) for p in held_out], 4)
            after = lib.speculation_counters(server.metrics())
            record["serve"][name] = {"ready_seconds": round(ready, 1), "ok": sum(1 for r in results if r.get("ok")), "requests": len(results), "acceptance": lib.acceptance(before, after), "sleep_cycle": cycle}
            ctx.progress(f"served-{name}", record, ready_seconds=round(ready, 1), ok=record["serve"][name]["ok"])
        finally:
            server.stop()
    answered = record["regenerate"]["rows"] > 0 or bool(record.get("adopted", {}).get("digest_matches"))
    record["gate_passed"] = not record["skipped"] and all(v["ok"] == v["requests"] and v["acceptance"] is not None for v in record["serve"].values()) and answered
    record["ended_at"] = now()
    return record


def stage_diagnose(ctx: Context) -> dict:
    """Which part of the draft pipeline loses acceptance: the published draft as its publisher
    ships it, the same draft after `speculators convert` (both settings of
    norm_before_residual), and the smoke stage's 10-step draft, each served by vLLM in eager
    mode on the same 16 held-out prompts, acceptance read from vLLM's own counters."""
    record: dict = {"stage": "diagnose", "started_at": now(), "arms": {}, "skipped": []}
    plan = ctx.plan
    held_out = read_prompts(ctx, "held_out")[:16 if not ctx.dry else 4]
    work = (ctx.run_dir if ctx.dry else Path("/tmp")) / "diagnose"
    arms = [("published", plan.arm_args("published", ""))]
    for name, kwargs in (("converted", None), ("converted-norm-before-residual", '{"norm_before_residual": true}')):
        target = work / name
        if not (target / "config.json").exists():
            extra = ["--algorithm-kwargs", kwargs] if kwargs else []
            code, _ = run_speculators(ctx, ["convert", ctx.published_dir, "--algorithm", "eagle3", "--verifier", ctx.model_dir, "--output-path", str(target), *extra], f"convert-{name}", 600)
            if code != 0:
                record["skipped"].append(f"{name}: convert exited {code}")
                continue
        arms.append((name, plan.arm_args("trained", str(target))))
    if (ctx.run_dir / "draft" / "config.json").exists():
        arms.append(("smoke-trained", plan.arm_args("trained", str(ctx.run_dir / "draft"))))
    for offset, (name, args) in enumerate(arms):
        if ctx.time_left() < 3 * 60:
            record["skipped"].append(f"{name}: past the stage deadline")
            continue
        server = ctx.server(name, 8101 + offset, "measure", ["--enforce-eager", *args], {"accept": 0.6})
        try:
            ready = server.wait_ready(600)
            before = lib.speculation_counters(server.metrics())
            results, seconds = load(server.url, [lib.chat_request(plan.model, p["content"], 256, stream=True) for p in held_out], 4)
            after = lib.speculation_counters(server.metrics())
            record["arms"][name] = {"ready_seconds": round(ready, 1), "ok": sum(1 for r in results if r.get("ok")), "requests": len(results), "seconds": round(seconds, 1), "acceptance": lib.acceptance(before, after)}
        except RuntimeError as error:
            record["arms"][name] = {"error": str(error)[-1500:]}
        finally:
            server.stop()
        ctx.progress(f"served-{name}", record, **{k: v for k, v in record["arms"][name].items() if k in ("ready_seconds", "ok", "error")})
    record["all_arms_measured"] = bool(record["arms"]) and all("acceptance" in v for v in record["arms"].values())
    record["ended_at"] = now()
    return record


def stage_train(ctx: Context) -> dict:
    if ctx.plan.adopt_draft:
        raise RuntimeError(f"plan {ctx.plan.name} adopts another run's draft (`trained_draft.py adopt`) and trains nothing")
    record: dict = {"stage": "train", "started_at": now()}
    started = time.time()
    rows = ctx.run_dir / "train" / "rows.jsonl"
    prompts = read_prompts(ctx, "train")
    if ctx.plan.train_prompts:
        prompts = random.Random(lib.SEED).sample(prompts, min(ctx.plan.train_prompts, len(prompts)))
    if ctx.dry:
        prompts = prompts[:200]
    record["regenerate"] = regenerate(ctx, prompts, rows)
    ctx.progress("answers", record, rows=record["regenerate"]["rows"], seconds=record["regenerate"]["seconds"])
    # Training gets what is left before the deadline, at most TRAIN_LOOP_SECONDS, less time
    # to stop the trainer, copy the draft and commit.
    record["train"] = train_draft(ctx, rows, max_steps=None, loop_seconds=max(60.0, min(ctx.plan.train_loop_seconds, ctx.time_left() - 4 * 60)))
    record["training_facts"] = {"examples": record["regenerate"]["rows"], "training_tokens": record["regenerate"]["answer_tokens"], "dataset_fingerprint": record["regenerate"]["fingerprint"],
                                "produced_at": now(), "wall_seconds": round(time.time() - started, 1)}
    record["gate_passed"] = record["regenerate"]["rows"] > 0 and (ctx.run_dir / "draft" / "config.json").exists()
    record["ended_at"] = now()
    return record


def bodies_for(model: str, prompts: list[dict], start: int, count: int, max_tokens: int) -> list[dict]:
    return [lib.chat_request(model, prompts[(start + i) % len(prompts)]["content"], max_tokens, stream=True) for i in range(count)]


def ids_for(prompts: list[dict], start: int, count: int) -> list[str]:
    """The ids bodies_for sends, in its order (the traffic shape names prompts, never text)."""
    return [prompts[(start + i) % len(prompts)]["id"] for i in range(count)]


STANDIN_BEHAVIOUR = {
    "none": {"tokens_per_second": 400, "answer_tokens": 160}, "none_b": {"tokens_per_second": 400, "answer_tokens": 160}, "ngram": {"tokens_per_second": 430, "accept": 0.2, "answer_tokens": 160},
    "suffix": {"tokens_per_second": 450, "accept": 0.3, "answer_tokens": 160}, "published": {"tokens_per_second": 560, "accept": 0.55, "answer_tokens": 160},
    "trained": {"tokens_per_second": 620, "accept": 0.62, "answer_tokens": 160},
}


def measure_window(ctx: Context, window: lib.Window, held_out: list[dict], items: list[dict], timed_only: bool = False) -> dict:
    """One window: its servers start one after another and then all serve at once on the GPU;
    only one works at a time. The control runs first and last, every candidate run between two
    control runs, every run of a load on the same held-out prompts. timed_only (the confirm
    stage) skips the identical-answer pass and the task set."""
    plan = ctx.plan
    record: dict = {"control": window.control, "candidates": list(window.candidates), "skipped": [], "ready_seconds": {}, "loads": [], "acceptance": {}, "held_out_acceptance": {},
                    "acceptance_readings": {}, "held_out_readings": {}}
    shared = ["--no-enable-prefix-caching", "--max-num-seqs", "64"]
    arms: dict[str, Server] = {}
    awake: list[str] = []

    def use(name: str) -> Server:
        """With sleep mode only one server holds the GPU: wake it, put the others to sleep."""
        if plan.sleep_mode and awake != [name]:
            for other in awake:
                record["sleep_seconds"].append(round(arms[other].sleep(), 2))
            awake.clear()
            record["wake_seconds"].append(round(arms[name].wake(), 2))
            awake.append(name)
        return arms[name]

    record["sleep_seconds"], record["wake_seconds"] = [], []
    try:
        for offset, name in enumerate((window.control, *window.candidates)):
            arms[name] = ctx.server(name, 8100 + offset, "measure", [*shared, *plan.arm_args(name, str(ctx.run_dir / "draft"))], STANDIN_BEHAVIOUR[name])
            record["ready_seconds"][name] = round(arms[name].wait_ready(900), 1)
            ctx.progress(f"ready-{name}", seconds=record["ready_seconds"][name])
            if plan.sleep_mode:
                # The next server measures its free memory at start, so this one sleeps first.
                record["sleep_seconds"].append(round(arms[name].sleep(), 2))
        warm_offset, offset = len(held_out) // 2, 0
        for load_level in plan.loads:
            if ctx.time_left() < 0:
                record["skipped"].append(f"load {load_level}: past the stage deadline")
                continue
            count = plan.requests_per_run[load_level] if not ctx.dry else 4
            warm = max(16, 2 * load_level) if not ctx.dry else 4
            for name in arms:
                load(use(name).url, bodies_for(plan.model, held_out, warm_offset, warm, 64), load_level)
            warm_ids = ids_for(held_out, warm_offset, warm)
            warm_offset += warm
            prompts = bodies_for(plan.model, held_out, offset, count, plan.timing_max_tokens)
            prompt_ids = ids_for(held_out, offset, count)
            offset += count
            runs, readings = [], {name: [] for name in arms if name != "none"}
            for arm in lib.window_schedule(list(window.candidates), RUNS_PER_CANDIDATE):
                setup = window.control if arm == "control" else arm
                server = use(setup)
                before = lib.speculation_counters(server.metrics()) if setup in readings else None
                results, seconds = load(server.url, prompts, load_level)
                if before is not None:
                    readings[setup].append((before, lib.speculation_counters(server.metrics())))
                summary = lib.summarize_run("control" if arm == "control" else "candidate", results, seconds)
                summary["setup"] = setup
                runs.append(summary)
            record["loads"].append({"concurrency": load_level, "requests_per_run": count, "max_tokens": plan.timing_max_tokens, "same_prompts_every_run": True,
                                    "warm_ids": warm_ids, "prompt_ids": prompt_ids, "runs": runs})
            ctx.progress(f"window-{window.control}-at-{load_level}", runs=len(runs), failed=sum(r["failed"] for r in runs))
            for name, pairs in readings.items():
                record["acceptance"].setdefault(name, {})[str(load_level)] = lib.acceptance(pairs[0][0], pairs[-1][1]) if pairs else None
                if pairs:
                    record["acceptance_readings"].setdefault(name, {})[str(load_level)] = {"before": pairs[0][0], "after": pairs[-1][1]}
        if timed_only:
            return record
        same = held_out[:plan.same_answer_prompts] if not ctx.dry else held_out[:12]
        answers = {}
        labels = [(window.control, window.control), (window.control + "_again", window.control), *((name, name) for name in window.candidates)]
        for label, arm in labels:
            if ctx.time_left() < 0:
                record["skipped"].append(f"same answers on {label}: past the stage deadline")
                continue
            before = lib.speculation_counters(use(arm).metrics())
            results, _ = load(arms[arm].url, [lib.chat_request(plan.model, p["content"], plan.same_answer_max_tokens, stream=True) for p in same], 16 if not ctx.dry else 4)
            if arm != "none" and not label.endswith("_again"):
                after = lib.speculation_counters(arms[arm].metrics())
                record["held_out_acceptance"][arm] = lib.acceptance(before, after)
                record["held_out_readings"][arm] = {"before": before, "after": after}
            answers[label] = [r.get("text") if r.get("ok") else None for r in results]
        record["same_answers"] = {"prompts": len(same), "max_tokens": plan.same_answer_max_tokens, "ids": [p["id"] for p in same], "answers": answers}
        if items:
            task_answers = {}
            for arm in (window.control, *window.candidates):
                if ctx.time_left() < 0:
                    record["skipped"].append(f"task set on {arm}: past the stage deadline")
                    continue
                results, _ = load(use(arm).url, [lib.chat_request(plan.model, item["prompt"], int(item.get("max_tokens") or 1024), stream=True) for item in items], TASK_CONCURRENCY)
                task_answers[arm] = [r.get("text") if r.get("ok") else None for r in results]
            record["tasks"] = {"file_sha256": QUALITY_SHA256, "items": [{"id": i["id"], "kind": i["kind"], "gold": i["gold"]} for i in items], "answers": task_answers,
                               "rule": plan.task_rule, "passes": []}
    finally:
        for server in arms.values():
            server.stop()
    if items and plan.task_servers > 1:
        # More servers of each setup answer the task set, each pass on freshly started servers, so
        # the item-level differences of several server pairs are pooled (the product's rule for
        # setups that change answers reads a 1-point loss; one pair cannot show it).
        spec = [name for name in (window.control, *window.candidates) if name != "none_b"]
        for number in range(2, plan.task_servers + 1):
            if ctx.time_left() < 0:
                record["skipped"].append(f"task set pass {number}: past the stage deadline")
                continue
            servers = {}
            try:
                for offset, name in enumerate(spec):
                    servers[name] = ctx.server(f"{name}-pass{number}", 8200 + offset, "measure", ["--no-enable-prefix-caching", "--max-num-seqs", "64", *plan.arm_args(name, str(ctx.run_dir / "draft"))], STANDIN_BEHAVIOUR[name])
                    servers[name].wait_ready(900)
                answers = {}
                for name, server in servers.items():
                    results, _ = load(server.url, [lib.chat_request(plan.model, item["prompt"], int(item.get("max_tokens") or 1024), stream=True) for item in items], TASK_CONCURRENCY)
                    answers[name] = [r.get("text") if r.get("ok") else None for r in results]
                record["tasks"]["passes"].append(answers)
                ctx.progress(f"task-pass-{number}", record, arms=len(answers))
            finally:
                for server in servers.values():
                    server.stop()
    return record


def stage_measure(ctx: Context) -> dict:
    plan = ctx.plan
    record: dict = {"schema": lib.RECORD_SCHEMA, "stage": "measure", "plan": plan.name, "rules": plan.rules, "started_at": now(), "model": plan.model, "model_revision": plan.model_revision,
                    "engine": f"vllm {lib.VLLM_VERSION}", "image": lib.VLLM_IMAGE, "gpu": plan.gpu, "draft_tokens": lib.DRAFT_TOKENS,
                    "flags_shared_by_every_arm": ["--no-enable-prefix-caching", "--max-num-seqs 64", f"--max-model-len {plan.max_model_len['measure']}", f"--gpu-memory-utilization {plan.memory_share['measure']}"],
                    "arm_flags": {name: plan.arm_args(name, "TRAINED_DRAFT_DIR") for name in ("none", "ngram", "suffix", "published", "trained")},
                    "precision": plan.precision, "commit": ctx.harness.get("commit", ""), "dirty": bool(ctx.harness.get("dirty", True)),
                    "command": f"TRAINED_DRAFT_PLAN={plan.name} modal run tools/trained-draft/trained_draft.py::launch --stage measure --run-id {ctx.run_dir.name} "
                               f"--budget-usd {lib.dollars(plan.stage_seconds['measure'], plan.dollars_per_hour)}"}
    held_out = read_prompts(ctx, "held_out")
    items = []
    if plan.task_items:
        everything = json.loads(task_set_file().read_text())["items"]
        # An even stride keeps both halves of the set (GSM8K, then MMLU) in a smaller sample.
        items = everything[::max(1, len(everything) // plan.task_items)][:plan.task_items]
        items = items if not ctx.dry else items[:40]
    record["machine"] = machine()
    if (ctx.run_dir / "report.json").exists():
        # The draft served is the one its training report names (an adopted draft especially).
        record["draft_digest_matches"] = draft_digest_matches(ctx)
        if not record["draft_digest_matches"]:
            raise RuntimeError("the draft in this run is not the one its training report names")
    record["task_servers"] = plan.task_servers
    record["windows"] = [measure_window(ctx, window, held_out, items) for window in plan.windows]
    # The draft-acceptance evidence and the traffic shape, digested by the contract record.
    record["evidence_files"] = lib.write_evidence(ctx.run_dir, record)
    record["ended_at"] = now()
    return record


def draft_digest_matches(ctx: Context) -> bool:
    """Whether the run's draft files hash to the artifact digest its training report states."""
    _, digest = lib.file_manifest(ctx.run_dir / "draft")
    return digest == json.loads((ctx.run_dir / "report.json").read_text()).get("artifact_digest")


def adopted_draft(ctx: Context) -> dict:
    """The adopted draft's facts: it is there, and it is the one its training report names."""
    if not (ctx.run_dir / "draft" / "config.json").exists() or not (ctx.run_dir / "report.json").exists():
        raise RuntimeError("an adopting plan needs `trained_draft.py adopt` first: the draft, its report and its training record in this run")
    report = json.loads((ctx.run_dir / "report.json").read_text())
    return {"artifact_digest": report.get("artifact_digest"), "digest_matches": draft_digest_matches(ctx), "base_model": report.get("base_model")}


def machine() -> dict:
    """Which machine a stage ran on: the container (Modal's task id), the host and the GPU's
    UUID, so the judge can tell a confirm window ran somewhere other than the measure window."""
    gpu = ""
    try:
        gpu = subprocess.run(["nvidia-smi", "--query-gpu=uuid", "--format=csv,noheader"], capture_output=True, text=True, check=False, timeout=30).stdout.strip()  # noqa: S607
    except (OSError, subprocess.SubprocessError):
        pass
    task = os.environ.get("MODAL_TASK_ID", "")
    # A host name is recorded only inside a Modal container; a laptop's names its owner.
    return {"task_id": task, "hostname": socket.gethostname() if task else "", "gpu_uuid": gpu}


def start_once(run_dir: Path, stage: str, call_id: str, commit=None) -> dict | None:
    """A stage runs once per launch. Modal restarts a preempted call on a new container by
    itself, and with the launcher detached nothing would hold that rerun to the stage's cap, so
    every start is written to the volume by call id first, and a second start of the same call
    refuses itself (seconds of cost) and says so; a new launch is a new call and runs."""
    run_dir.mkdir(parents=True, exist_ok=True)
    path = run_dir / f"{stage}-starts.jsonl"
    earlier = [json.loads(line) for line in path.read_text().splitlines() if line.strip()] if path.exists() else []
    with path.open("a") as handle:
        handle.write(json.dumps({"at": now(), "call_id": call_id, "machine": machine()}) + "\n")
    if commit is not None:
        commit()
    if call_id and any(start.get("call_id") == call_id for start in earlier):
        return {"stage": stage, "ran": False, "gate_passed": False, "refused_rerun": True,
                "error": "Modal restarted this call after a preemption; a stage runs once per launch, so launch it again (the earlier start's spend is lost)"}
    return None


def stage_confirm(ctx: Context) -> dict:
    """The fresh-machine confirmation: in a container other than the measure stage's, the trained
    draft against the control of the window it was measured in, at the same loads, timed runs
    only. The judge attaches this record's sha256 as fresh-machine-confirmation where it measured
    the gain again."""
    plan = ctx.plan
    measured = ctx.run_dir / "measure.json"
    if not measured.exists() or not (ctx.run_dir / "draft" / "config.json").exists():
        raise RuntimeError("confirm needs the measure stage's record and the trained draft in the run")
    window = next(w for w in plan.windows if "trained" in w.candidates)
    here, first = machine(), (json.loads(measured.read_text()).get("machine") or {}).get("gpu_uuid", "")
    if not ctx.dry:
        # The judge counts this window only from another GPU; a refusal here costs a container start.
        if not first:
            raise RuntimeError("the measure stage did not record its GPU, so no window can be a fresh machine's against it")
        if here["gpu_uuid"] == first:
            raise RuntimeError(f"this container has the measure stage's GPU ({first}); launch confirm again to get another")
    record: dict = {"schema": lib.CONFIRM_SCHEMA, "stage": "confirm", "plan": plan.name, "rules": plan.rules, "started_at": now(), "model": plan.model, "model_revision": plan.model_revision,
                    "engine": f"vllm {lib.VLLM_VERSION}", "image": lib.VLLM_IMAGE, "gpu": plan.gpu, "draft_tokens": lib.DRAFT_TOKENS,
                    "confirms": "sha256:" + hashlib.sha256(measured.read_bytes()).hexdigest(), "machine": here, "measure_gpu_uuid": first,
                    "commit": ctx.harness.get("commit", ""), "dirty": bool(ctx.harness.get("dirty", True))}
    record["windows"] = [measure_window(ctx, lib.Window(window.control, ("trained",)), read_prompts(ctx, "held_out"), [], timed_only=True)]
    record["gate_passed"] = all(not w["skipped"] and len(w["loads"]) == len(plan.loads) for w in record["windows"])
    record["ended_at"] = now()
    return record


def write_report(ctx: Context, train_record: dict, measure_record: dict) -> dict:
    """The training report, with held-out acceptance measured by the serving engine on the
    answer pass's held-out prompts: the trained draft's, and the published draft's it
    started from."""
    facts = train_record["training_facts"]
    measured: dict = {}
    prompts = 0
    for window in measure_record.get("windows", []):
        measured.update({k: v for k, v in window.get("held_out_acceptance", {}).items() if v})
        prompts = max(prompts, window.get("same_answers", {}).get("prompts", 0))
    trained, published = measured.get("trained") or {}, measured.get("published") or {}
    report = lib.training_report(lib.TrainingFacts(
        name="oasst1-chat", examples=facts["examples"], held_out_examples=prompts, training_tokens=facts["training_tokens"],
        dataset_fingerprint=facts["dataset_fingerprint"], held_out_acceptance=trained.get("per_position") or [], start_acceptance=published.get("per_position") or [],
        produced_at=facts["produced_at"], wall_seconds=facts["wall_seconds"]), ctx.run_dir / "draft", ctx.plan)
    lib.write_json(ctx.run_dir / "report.json", report)
    return report


def now() -> str:
    return datetime.now(timezone.utc).replace(microsecond=0).isoformat().replace("+00:00", "Z")


def harness_commit() -> dict:
    """The commit this harness runs from and whether tools/trained-draft differs from it.
    Unknown (empty, dirty) outside a git checkout."""
    try:
        commit = subprocess.run(["git", "rev-parse", "HEAD"], cwd=REPOSITORY, capture_output=True, text=True, check=True).stdout.strip()  # noqa: S607
        changed = subprocess.run(["git", "status", "--porcelain", "--", "tools/trained-draft"], cwd=REPOSITORY, capture_output=True, text=True, check=True).stdout.strip()  # noqa: S607
    except (OSError, subprocess.CalledProcessError):
        return {"commit": "", "dirty": True}
    return {"commit": commit, "dirty": bool(changed)}


def run_stage(ctx: Context, stage: str) -> dict:
    ctx.stage = stage
    ctx.progress("start", plan=ctx.plan.name)
    record = {"smoke": stage_smoke, "train": stage_train, "measure": stage_measure, "diagnose": stage_diagnose, "confirm": stage_confirm}[stage](ctx)
    # "ran" says the stage finished; whether it opens the next stage is "gate_passed".
    record["ran"] = True
    lib.write_json(ctx.run_dir / f"{stage}.json", record)
    ctx.progress("end", gate_passed=record.get("gate_passed"))
    if stage == "measure":
        record["report"] = write_report(ctx, json.loads((ctx.run_dir / "train.json").read_text()), record)
    return record


# ---------------------------------------------------------------------------
# Modal (paid). Imported only where modal is installed.
# ---------------------------------------------------------------------------

try:
    import modal
except ImportError:  # the laptop's dry run needs no Modal
    modal = None

if modal is not None:
    app = modal.App(f"{APP_NAME}-{PLAN.name}")
    volume = modal.Volume.from_name(VOLUME_NAME, create_if_missing=True)
    image = (
        modal.Image.from_registry(lib.VLLM_IMAGE, setup_dockerfile_commands=["RUN ln -sf /usr/bin/python3 /usr/local/bin/python"]).entrypoint([])
        .apt_install("build-essential")
        .run_commands("python3 -m pip install uv==0.13.9 || python3 -m pip install --break-system-packages uv==0.13.9")
        # speculators keeps its own torch and transformers, apart from vLLM's.
        .run_commands("uv venv /opt/spec --python 3.12", f"uv pip install --python /opt/spec/bin/python speculators=={lib.SPECULATORS_VERSION}")
        .env({"HF_HOME": "/vol/hf", "HF_HUB_DISABLE_TELEMETRY": "1", "VLLM_NO_USAGE_STATS": "1", "DO_NOT_TRACK": "1", lib.PLAN_VARIABLE: PLAN.name})
        .add_local_file(str(QUALITY_FILE), "/root/tasks.json")
        .add_local_file(str(HERE / "rung1_offline.py"), "/root/rung1_offline.py")
        .add_local_file(str(HERE / "trainer_view.py"), "/root/trainer_view.py")
        .add_local_file(str(HERE / "draft_embedding.py"), "/root/draft_embedding.py")
        # Modal includes this file itself; its one local import is added as source.
        .add_local_python_source("draftlib")
    )
    download_image = modal.Image.debian_slim(python_version="3.12").pip_install("huggingface_hub[hf_xet]==1.1.4").env({lib.PLAN_VARIABLE: PLAN.name}).add_local_python_source("draftlib")

    @app.function(image=download_image, volumes={str(VOLUME_ROOT): volume}, cpu=2, memory=8192, timeout=1800, retries=0)
    def download() -> dict:
        """Stages the plan's pinned weights on the volume (CPU only, no GPU time)."""
        from huggingface_hub import snapshot_download

        out = {}
        for repository, revision in ((PLAN.model, PLAN.model_revision), (PLAN.published, PLAN.published_revision)):
            target = VOLUME_ROOT / "weights" / weights_name(repository)
            marker = target / ".complete"
            if not (marker.exists() and marker.read_text() == revision):
                snapshot_download(repository, revision=revision, local_dir=str(target))
                marker.write_text(revision)
            out[repository] = revision
        volume.commit()
        return out

    def _gpu_stage(stage: str, run_id: str, harness: dict) -> dict:
        global QUALITY_FILE
        QUALITY_FILE = Path("/root/tasks.json")
        run_dir = VOLUME_ROOT / "runs" / run_id
        started = time.time()
        volume.reload()  # a restarted container must see the first start's line
        refused = start_once(run_dir, stage, modal.current_function_call_id() or "", volume.commit)
        if refused is not None:
            lib.write_json(run_dir / f"{stage}-summary.json", refused)
            volume.commit()
            return refused
        ctx = Context(run_dir, dry=False, weights=VOLUME_ROOT / "weights", speculators="/opt/spec/bin/speculators", deadline=started + PLAN.stage_seconds[stage] - DEADLINE_MARGIN_SECONDS, commit=volume.commit,
                      harness=harness)
        try:
            record = run_stage(ctx, stage)
        except Exception as error:  # noqa: BLE001 - the failure is the record
            record = {"stage": stage, "ran": False, "gate_passed": False, "error": f"{type(error).__name__}: {error}"[:4000]}
            lib.write_json(run_dir / f"{stage}.json", record)
        record["gpu_seconds"] = round(time.time() - started, 1)
        record["gpu_facts"] = subprocess.run(["nvidia-smi", "--query-gpu=name,driver_version,memory.total", "--format=csv,noheader"], capture_output=True, text=True, check=False).stdout.strip()  # noqa: S607
        record["estimated_dollars"] = lib.dollars(record["gpu_seconds"], PLAN.dollars_per_hour)
        lib.write_json(run_dir / f"{stage}-summary.json", {k: v for k, v in record.items() if k in ("stage", "ran", "gate_passed", "error", "gpu_seconds", "gpu_facts", "estimated_dollars", "started_at", "ended_at")})
        volume.commit()
        return {k: v for k, v in record.items() if k in ("stage", "ran", "gate_passed", "error", "gpu_seconds", "gpu_facts", "estimated_dollars")}

    # One function per stage, so Modal's timeout is each stage's own hard cap.
    gpu = {"image": image, "gpu": "H100!" if PLAN.gpu == "H100" else PLAN.gpu, "cpu": PLAN.gpu_cpu, "memory": PLAN.gpu_memory_mib, "volumes": {str(VOLUME_ROOT): volume}, "retries": 0}

    @app.function(**gpu, timeout=PLAN.stage_seconds["smoke"])
    def gpu_smoke(run_id: str, commit: str = "", dirty: bool = True) -> dict:
        return _gpu_stage("smoke", run_id, {"commit": commit, "dirty": dirty})

    @app.function(**gpu, timeout=PLAN.stage_seconds["train"])
    def gpu_train(run_id: str, commit: str = "", dirty: bool = True) -> dict:
        return _gpu_stage("train", run_id, {"commit": commit, "dirty": dirty})

    @app.function(**gpu, timeout=PLAN.stage_seconds["measure"])
    def gpu_measure(run_id: str, commit: str = "", dirty: bool = True) -> dict:
        return _gpu_stage("measure", run_id, {"commit": commit, "dirty": dirty})

    @app.function(**gpu, timeout=PLAN.stage_seconds["confirm"])
    def gpu_confirm(run_id: str, commit: str = "", dirty: bool = True) -> dict:
        return _gpu_stage("confirm", run_id, {"commit": commit, "dirty": dirty})

    @app.function(**gpu, timeout=RUNG1_SECONDS)
    def gpu_rung1(run_id: str, commit: str = "", dirty: bool = True) -> dict:
        """Rung 1 on the GPU: rung1_offline.py with its working files on the container's disk,
        and its record, logs and answers' token counts copied to the volume."""
        if PLAN.name != "l4-qwen3-1.7b":
            return {"stage": "rung1", "ran": False, "error": "rung 1 belongs to plan l4-qwen3-1.7b"}
        run_dir, work = VOLUME_ROOT / "runs" / run_id, Path("/tmp/rung1")
        started = time.time()
        work.mkdir(parents=True, exist_ok=True)
        if (run_dir / "rung1" / "answers.json").exists():
            # A rerun keeps the greedy answers of the first run and redoes everything after them.
            shutil.copy2(run_dir / "rung1" / "answers.json", work / "answers.json")
        target = run_dir / "rung1"
        target.mkdir(parents=True, exist_ok=True)
        # The script's own progress lines go straight to the volume, so a cut run leaves them.
        with (target / "stdout.log").open("w") as handle:
            try:
                completed = subprocess.run(["/opt/spec/bin/python", "/root/rung1_offline.py", "--run-dir", str(work), "--speculators", "/opt/spec/bin/speculators", "--prompts", str(run_dir / "prompts")],  # noqa: S603
                                           stdout=handle, stderr=subprocess.STDOUT, timeout=RUNG1_SECONDS - 3 * 60, check=False)
                code = completed.returncode
            except subprocess.TimeoutExpired:
                code = -1
                handle.write("stopped at the wall clock\n")
        for path in list(work.glob("*.json")) + list(work.glob("*.log")):
            shutil.copy2(path, target / path.name)
        record = json.loads((work / "rung1.json").read_text()) if (work / "rung1.json").exists() else {}
        bar = record.get("pass_bar") or {}
        checks = [value for key, value in bar.items() if key != "best_model_free_tokens_per_step"]
        summary = {"stage": "rung1", "ran": code == 0, "exit": code, "all_checks_pass": bool(checks) and all(value == "pass" for value in checks), "pass_bar": bar or None,
                   "embedding": record.get("embedding"),
                   "view": {k: (record.get("view") or {}).get(k) for k in ("draft_embedding_is_verifiers", "speculators_first_token_accuracy", "reference_first_token_accuracy",
                                                                            "forwards_agree", "verifier_predicts_next_token")},
                   "gpu_seconds": round(time.time() - started, 1)}
        summary["estimated_dollars"] = lib.dollars(summary["gpu_seconds"], PLAN.dollars_per_hour)
        lib.write_json(target / "rung1-summary.json", summary)
        volume.commit()
        return summary

    def _gpu_diagnose(run_id: str) -> dict:
        run_dir = VOLUME_ROOT / "runs" / run_id
        started = time.time()
        ctx = Context(run_dir, dry=False, weights=VOLUME_ROOT / "weights", speculators="/opt/spec/bin/speculators", deadline=started + DIAGNOSE_SECONDS - 2 * 60, commit=volume.commit)
        try:
            record = run_stage(ctx, "diagnose")
        except Exception as error:  # noqa: BLE001 - the failure is the record
            record = {"stage": "diagnose", "ran": False, "error": f"{type(error).__name__}: {error}"[:4000]}
            lib.write_json(run_dir / "diagnose.json", record)
        record["gpu_seconds"] = round(time.time() - started, 1)
        record["estimated_dollars"] = lib.dollars(record["gpu_seconds"], PLAN.dollars_per_hour)
        lib.write_json(run_dir / "diagnose.json", record)
        volume.commit()
        return {k: v for k, v in record.items() if k in ("stage", "ran", "all_arms_measured", "error", "arms", "skipped", "gpu_seconds", "estimated_dollars")}

    @app.function(**gpu, timeout=DIAGNOSE_SECONDS)
    def gpu_diagnose(run_id: str, commit: str = "", dirty: bool = True) -> dict:
        return _gpu_diagnose(run_id)

    @app.function(image=download_image, volumes={str(VOLUME_ROOT): volume}, cpu=1, memory=1024, timeout=SELFTEST_SECONDS, retries=0)
    def cpu_selftest(run_id: str, overrun: bool = False) -> dict:
        """A CPU stand-in of a stage, for `modal run --detach` with the client killed: a progress
        line every 5 seconds on the volume, then a record after 45 seconds. With overrun it never
        ends on its own, so Modal's timeout ends it: the wall clock is enforced remotely."""
        run_dir = VOLUME_ROOT / "runs" / run_id
        run_dir.mkdir(parents=True, exist_ok=True)
        started = time.time()
        volume.reload()
        refused = start_once(run_dir, "selftest", modal.current_function_call_id() or "", volume.commit)
        if refused is not None:
            return refused
        while overrun or time.time() - started < 45:
            with (run_dir / "selftest-progress.jsonl").open("a") as handle:
                handle.write(json.dumps({"at": now(), "stage": "selftest", "phase": "alive", "seconds": round(time.time() - started, 1), "overrun": overrun}) + "\n")
            volume.commit()
            time.sleep(5)
        record = {"stage": "selftest", "ran": True, "gate_passed": True, "started_at": datetime.fromtimestamp(started, timezone.utc).isoformat(), "ended_at": now(), "machine": machine()}
        lib.write_json(run_dir / "selftest.json", record)
        volume.commit()
        return record

    GPU_STAGES = {"smoke": gpu_smoke, "train": gpu_train, "measure": gpu_measure, "confirm": gpu_confirm, "rung1": gpu_rung1, "diagnose": gpu_diagnose}

    @app.local_entrypoint()
    def launch(stage: str, run_id: str, budget_usd: float) -> None:
        """One paid stage of the plan in TRAINED_DRAFT_PLAN. Refuses a budget below the
        stage's hard cap, uploads the prompts, stages the weights, and cancels the call if it
        has not ended 15 minutes after its cap."""
        if stage in ("selftest", "selftest-overrun"):
            # CPU only, cents: proves a detached stage outlives its client and its wall is remote.
            call = cpu_selftest.spawn(run_id, stage == "selftest-overrun")
            print(f"{stage}: call {call.object_id}, timeout {SELFTEST_SECONDS} s on one CPU")
            try:
                print(json.dumps(call.get(timeout=SELFTEST_SECONDS + 300), indent=1))
            except TimeoutError:
                call.cancel()
                raise SystemExit(f"{stage}: cancelled at the wall-clock limit") from None
            return
        if stage == "confirm":
            refusal = confirm_gate(run_id)
            if refusal:
                raise SystemExit(f"confirm refused: {refusal}")
        seconds = {"rung1": RUNG1_SECONDS, "diagnose": DIAGNOSE_SECONDS}.get(stage) or PLAN.stage_seconds[stage]
        cap = lib.dollars(seconds, PLAN.dollars_per_hour)
        if budget_usd < cap:
            raise SystemExit(f"{PLAN.name} {stage} needs --budget-usd {cap} (its hard cap at ${PLAN.dollars_per_hour}/h for {seconds // 60} min)")
        local = LOCAL_RUNS / run_id
        if PLAN.task_items and hashlib.sha256(task_set_file().read_bytes()).hexdigest() != QUALITY_SHA256:
            raise SystemExit(f"{QUALITY_FILE} is not the task set the rules name (sha256 {QUALITY_SHA256})")
        if not (local / "prompts" / "manifest.json").exists():
            raise SystemExit(f"run `python3 trained_draft.py prepare --run-id {run_id}` first")
        with volume.batch_upload(force=True) as batch:
            batch.put_directory(str(local / "prompts"), f"/runs/{run_id}/prompts")
        harness = harness_commit()
        print(json.dumps({"plan": PLAN.name, "harness": harness, "weights": download.remote()}))
        call = GPU_STAGES[stage].spawn(run_id, harness["commit"], harness["dirty"])
        print(f"{PLAN.name} {stage}: call {call.object_id}, hard cap ${cap}, wall clock {seconds // 60} min")
        try:
            # Modal's timeout ends the container at the cap; the wait adds time to schedule
            # it and pull the image, which bills no GPU.
            print(json.dumps(call.get(timeout=seconds + 900), indent=1))
        except TimeoutError:
            call.cancel()
            raise SystemExit(f"{stage}: cancelled at the wall-clock limit") from None


# ---------------------------------------------------------------------------
# Laptop commands ($0).
# ---------------------------------------------------------------------------


def dry_run(run_id: str, speculators: str, online: bool = False) -> dict:
    """Every stage on this machine: real oasst1 prompts, stand-in servers in place of
    vLLM, real speculators prepare-data and speculators' own --dry-run in place of
    training, then the judge on the record. With online, the smoke stage instead trains
    for real (10 steps) against the transformers stand-in of the extraction server, with
    the GPU run's exact trainer flags."""
    run_dir = LOCAL_RUNS / run_id
    if not (run_dir / "prompts" / "manifest.json").exists():
        prepare(run_dir)
    ctx = Context(run_dir, dry=True, weights=run_dir, speculators=speculators, dry_online=online, harness=harness_commit())
    out = {}
    stages = ("smoke",) if online else ("smoke", "train", "measure")
    for stage in stages:
        record = run_stage(ctx, stage)
        out[stage] = record.get("gate_passed", "ran" if record.get("ran") else "did not run")
    if online:
        out["train"] = json.loads((run_dir / "smoke.json").read_text())["train"]
        return out
    judged = subprocess.run(["go", "run", "./tools/trained-draft/judge", str(run_dir)], cwd=REPOSITORY, capture_output=True, text=True, check=False)  # noqa: S607
    (run_dir / "verdict.json").write_text(judged.stdout)
    out["judge_exit"] = judged.returncode
    out["judge_stderr"] = judged.stderr[-2000:]
    return out


STAGE_FILES = ("smoke", "train", "measure", "confirm", "diagnose", "selftest")


def adopt(source: str, run_id: str) -> dict:
    """A run starts from another run's trained draft: its draft, training report and training
    record are copied on the volume (no download), and the smoke and measure stages check the draft
    against the report's artifact digest. Refuses a run that already has a draft."""
    listing = subprocess.run(["modal", "volume", "ls", VOLUME_NAME, f"/runs/{run_id}"], capture_output=True, text=True, check=False)  # noqa: S607
    if f"runs/{run_id}/draft" in listing.stdout:
        raise SystemExit(f"run {run_id} already has a draft; adopt into a fresh run id")
    # The volume is a V1 volume: no recursive copy, so the draft's files are copied one by one, each
    # to its own path (a file-to-file copy creates the directories it needs).
    listed = subprocess.run(["modal", "volume", "ls", "--json", VOLUME_NAME, f"/runs/{source}/draft"], capture_output=True, text=True, check=False)  # noqa: S607
    files = [entry["filename"].rsplit("/", 1)[-1] for entry in (json.loads(listed.stdout) if listed.returncode == 0 and listed.stdout.strip() else []) if entry.get("type") == "file"]
    if "config.json" not in files:
        raise SystemExit(f"run {source} has no trained draft on the volume")
    for path in [f"draft/{name}" for name in files] + ["report.json", "train.json"]:
        done = subprocess.run(["modal", "volume", "cp", VOLUME_NAME, f"/runs/{source}/{path}", f"/runs/{run_id}/{path}"], capture_output=True, text=True, check=False)  # noqa: S607
        if done.returncode != 0:
            raise SystemExit(f"copying {path} from {source}: {done.stderr.strip()[-500:]}")
    target = LOCAL_RUNS / run_id
    target.mkdir(parents=True, exist_ok=True)
    subprocess.run(["modal", "volume", "get", "--force", VOLUME_NAME, f"/runs/{run_id}/report.json", str(target / "report.json")], capture_output=True, check=False)  # noqa: S607
    report = json.loads((target / "report.json").read_text()) if (target / "report.json").exists() else {}
    return {"from": source, "run": run_id, "artifact_digest": report.get("artifact_digest"), "base_model": report.get("base_model"), "examples": report.get("examples")}


def fetch(run_id: str, logs: bool = False) -> list[str]:
    """Rebuilds the run's local copy from the volume, with no client of the stage needed: every
    stage's record, summary and progress lines, the measure stage's evidence files and report,
    rung 1's files, and with logs the servers' and tools' logs. Returns the files it got."""
    target = LOCAL_RUNS / run_id
    names = [f"{stage}{suffix}" for stage in STAGE_FILES for suffix in (".json", "-summary.json", "-progress.jsonl", "-starts.jsonl")]
    names += ["report.json", lib.ACCEPTANCE_FILE, lib.SHAPE_FILE, "rung1/rung1.json", "rung1/rung1-summary.json", "rung1/stdout.log"]
    got = []
    for name in names:
        (target / name).parent.mkdir(parents=True, exist_ok=True)
        done = subprocess.run(["modal", "volume", "get", "--force", VOLUME_NAME, f"/runs/{run_id}/{name}", str(target / name)], capture_output=True, check=False)  # noqa: S607
        if done.returncode == 0 and (target / name).exists():
            got.append(name)
    if logs:
        subprocess.run(["modal", "volume", "get", "--force", VOLUME_NAME, f"/runs/{run_id}/logs", str(target)], capture_output=True, check=False)  # noqa: S607
    return got


def gate(run_id: str, stage: str) -> tuple[bool, dict]:
    """Whether a stage of the run may be followed by the next, read from the volume: smoke,
    train and confirm by their own summary's gate_passed; measure by the judge's reading of the
    rung's bar, the confirm stage's own precondition."""
    fetch(run_id)
    target = LOCAL_RUNS / run_id
    if stage == "measure":
        refusal = judged_refusal(target)
        return refusal == "", {"stage": "measure", "next_may_run": refusal == "", "why": refusal or "the rung's bar passed"}
    summary_path = target / f"{stage}-summary.json"
    if not summary_path.exists():
        return False, {"stage": stage, "next_may_run": False, "why": "no summary on the volume yet (the stage is running, never started, or was stopped)"}
    summary = json.loads(summary_path.read_text())
    if summary.get("refused_rerun"):
        summary["why"] = "Modal preempted the stage and its restart refused itself; launch the stage again"
    return summary.get("gate_passed") is True, summary


def judged_refusal(target: Path) -> str:
    """The judge on a fetched run ($0), and why the confirm stage may not follow it."""
    if not (target / "measure.json").exists():
        return "the measure stage left no record on the volume"
    env = {**os.environ, "ASDF_GOLANG_VERSION": os.environ.get("ASDF_GOLANG_VERSION", "1.26.6")}
    judged = subprocess.run(["go", "run", "./tools/trained-draft/judge", str(target)], cwd=REPOSITORY, capture_output=True, text=True, check=False, env=env)  # noqa: S607
    if judged.returncode != 0:
        return "the judge failed: " + judged.stderr[-800:]
    (target / "verdict.json").write_text(judged.stdout)
    return lib.confirm_refusal(json.loads(judged.stdout))


def confirm_gate(run_id: str) -> str:
    fetch(run_id)
    return judged_refusal(LOCAL_RUNS / run_id)


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    parser.add_argument("action", choices=["prepare", "dry-run", "fetch", "gate", "adopt"])
    parser.add_argument("--from", dest="source", help="adopt: the run whose trained draft this run serves")
    parser.add_argument("--stage", choices=["smoke", "train", "measure", "confirm", "selftest"], help="gate: the stage whose result decides the next")
    parser.add_argument("--logs", action="store_true", help="fetch: the servers' and tools' logs too")
    parser.add_argument("--run-id", required=True)
    parser.add_argument("--speculators", default=os.environ.get("SPECULATORS_BIN", "speculators"))
    parser.add_argument("--online", action="store_true", help="dry-run: train for real against the transformers extraction stand-in (smoke stage only)")
    options = parser.parse_args()
    if options.action == "prepare":
        print(json.dumps(prepare(LOCAL_RUNS / options.run_id), indent=1))
    elif options.action == "dry-run":
        print(json.dumps(dry_run(options.run_id, options.speculators, options.online), indent=1))
    elif options.action == "adopt":
        if not options.source:
            raise SystemExit("adopt needs --from RUN_ID")
        print(json.dumps(adopt(options.source, options.run_id), indent=1))
    elif options.action == "fetch":
        print(json.dumps({"run": options.run_id, "fetched": fetch(options.run_id, options.logs)}, indent=1))
    else:
        if not options.stage:
            raise SystemExit("gate needs --stage")
        passed, facts = gate(options.run_id, options.stage)
        print(json.dumps(facts, indent=1, default=str))
        return 0 if passed else 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
