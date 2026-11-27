"""One machine's part of a kernel-tuning proof: tune (first machine only), then serve the
current setup and the same setup with the tuned set in turn on this one GPU, control first
and last, four tuned runs, restarting the engine between runs. Writes the window record
`infercrane optimize evidence kernel-tuning` judges, and a detail file beside it.

Standard library only: the Modal function runs on its own Python and starts the engine and
the tuner with the image's own executables. `dry` swaps the engine for stand_in_server.py
and the GPU for the tuner's stand-in timings, keeping every other step; nothing a dry run
reports is a measurement.

The plan's numbers are fixed by rules-qwen36-moe-h100-2026-11-26.md, written before any
server started.
"""

from __future__ import annotations

import hashlib
import json
import os
import re
import shutil
import signal
import subprocess
import sys
import time
from pathlib import Path

import loadgen
import tune_moe

WINDOW_SCHEMA = "infercrane.kernel-tuning-window/v1"
DETAIL_SCHEMA = "infercrane.kernel-tuning-detail/v1"
RUNS_PER_ARM = 4  # the campaign's bracketing: C T C T C T C T C

PROMPT_FILES = {"v1": ("/fasttrack/prompts/prompts-v1.json", "c3c912dfb64e963fdae49f175bc2e400feef6a73a1210a70f5fe7d9f6d84c989"),
                "v2": ("/fasttrack/prompts/prompts-v2.json", "2f6b05635659520db775d85d218d0b2ded0104cecc634183e318fe87d4fac984"),
                "quality": ("/fasttrack/prompts/quality-v1.json", "83c3df7b82f8d8a3b9fa892f1fdcd81679f93646337c8b78d456a828101d72e4")}
# (cell, prompt file, path in it, requests at once): the map's short chat at 128 and 32.
MAP_SHORT_CELLS = (("short/128", "v2", ("kinds", "short", "levels", "128"), 128),
                   ("short/32", "v1", ("shapes", "short", "levels", "32"), 32))


def model_entry(model_id: str, revision: str) -> dict:
    return {"id": model_id, "revision": revision, "dir": f"/weights/{model_id.split('/', 1)[1].lower()}-{revision[:12]}"}


# A plan is one rung's fixed choices; its rules file states them before any server starts.
# KERNEL_TUNE_PLAN picks one, on the laptop and in the container alike.
PLANS = {
    # Rung 4: rules-qwen36-moe-h100-2026-11-26.md (launched 26 Nov from 8f579b02).
    "qwen36-moe-h100": {
        "rules": "rules-qwen36-moe-h100-2026-11-26.md", "gpu": "H100!", "expect_gpu": "H100 80GB HBM3",
        "model": model_entry("Qwen/Qwen3.6-35B-A3B-FP8", "c653bba46dfa06e9658dcc0b0dc87fe4685f14a2"),
        # vLLM recipes, Qwen3.6-35B-A3B (fc2e8151, 2026-09-14): base args --trust-remote-code, the
        # FP8 variant for one GPU, the Rust front end by default, and the documented text-only
        # option. --max-num-seqs 512 as the map's H100 runs added it.
        "serve_args": ["--trust-remote-code", "--language-model-only", "--max-num-seqs", "512"], "serve_env": {"VLLM_USE_RUST_FRONTEND": "1"},
        # The engine's step budget on an H100 is 8,192 tokens (vLLM 0.34.0 arg_utils, >= 70 GB).
        "loads": [{"running": 128, "batched_tokens": 8192}, {"running": 32, "batched_tokens": 8192}],
        "cells": MAP_SHORT_CELLS, "sequential_answers": 0, "limits": {"first": 75 * 60, "fresh": 50 * 60},
        "device": "NVIDIA_H100_80GB_HBM3", "dry_moe": {"num_experts": 256, "moe_intermediate_size": 512},
        "weights": "fp8_w8a8, 128x128 blocks", "kv_cache": "bf16 (engine default)",
    },
    # Rung 3: rules-qwen3-30b-a3b-l40s-2026-11-26-v2.md (v1 never ran).
    "qwen3-30b-a3b-l40s": {
        "rules": "rules-qwen3-30b-a3b-l40s-2026-11-26-v2.md", "gpu": "L40S", "expect_gpu": "L40S",
        "model": model_entry("Qwen/Qwen3-30B-A3B-Instruct-2507-FP8", "5a5a776300a41aaa681dd7ff0106608ef2bc90db"),
        # vLLM recipes, Qwen3-30B-A3B: no base arguments. --max-model-len 4096 so the KV cache fits
        # beside 31 GB of weights on 48 GB; both arms carry it.
        "serve_args": ["--max-model-len", "4096"], "serve_env": {},
        # The engine's step budget below 70 GB is 2,048 tokens (vLLM 0.34.0 arg_utils).
        "loads": [{"running": 128, "batched_tokens": 2048}, {"running": 32, "batched_tokens": 2048}],
        "cells": MAP_SHORT_CELLS, "sequential_answers": 24, "limits": {"first": 70 * 60, "fresh": 50 * 60},
        "device": "NVIDIA_L40S", "dry_moe": {"num_experts": 128, "moe_intermediate_size": 768},
        "weights": "fp8_w8a8, 128x128 blocks", "kv_cache": "bf16 (engine default)",
    },
    # Rung 2: rules-moe-shapes-l40s-2026-11-26-v3.md (v1 and v2 ran once each, 26 Nov). Shapes only: no model is loaded.
    "moe-shapes-l40s": {
        "rules": "rules-moe-shapes-l40s-2026-11-26-v3.md", "kind": "shapes", "gpu": "L40S", "expect_gpu": "L40S", "device": "NVIDIA_L40S",
        # block FP8 (128x128), hidden 2048, top-8; `share` is rung 1's estimate of the MoE layer's
        # share of a decode step at 32 and 128 requests at once (amdahl.py, bytes).
        "shapes": [{"model": "Qwen/Qwen3-30B-A3B-Instruct-2507-FP8", "experts": 128, "intermediate": 768, "hidden": 2048, "topk": 8,
                    "share": {32: 0.92, 128: 0.874}, "rung3": True},
                   {"model": "Qwen/Qwen3.6-35B-A3B-FP8", "experts": 256, "intermediate": 512, "hidden": 2048, "topk": 8,
                    "share": {32: 0.733, 128: 0.614}, "rung3": False}],
        "loads": [{"running": 128, "batched_tokens": 2048}, {"running": 32, "batched_tokens": 2048}],
        "tune_budget_seconds": 420.0, "limits": {"shapes": 25 * 60},
        "model": None, "serve_args": [], "serve_env": {}, "cells": (), "sequential_answers": 0, "dry_moe": {},
    },
}
PLAN_ID = os.environ.get("KERNEL_TUNE_PLAN", "qwen36-moe-h100")
PLAN = PLANS[PLAN_ID]
MODEL = PLAN["model"]
SERVE_ARGS, SERVE_ENV = PLAN["serve_args"], PLAN["serve_env"]
CELLS, LOADS = PLAN["cells"], PLAN["loads"]
EXPECT_GPU = PLAN["expect_gpu"]
PORT = 8000
MAX_TOKENS = 295  # the map's short-chat answer limit
QUALITY_AT_ONCE = 64
QUALITY_RUNS = {0: "control-1", 1: "candidate-1", 2: "control-2"}  # run index -> answer set name
WARMUP_SECONDS, WINDOW_SECONDS, COOLDOWN_SECONDS = 15.0, 45.0, 20.0
STARTUP_SECONDS = 15 * 60  # the first start compiles; later ones reuse the cache
TUNE_BUDGET_SECONDS, TUNE_PER_KEY_SECONDS = 540.0, 45.0
RUNTIME, VERSION = "vllm", "0.34.0"
TUNED_FOLDER_VARIABLE = "VLLM_TUNED_CONFIG_FOLDER"


# ----------------------------------------------------------------------------- engine log

ANSI = re.compile(r"\x1b\[[0-9;]*[A-Za-z]")
USED = re.compile(r"Using configuration from (\S+) for MoE layer\.")
DEFAULT = re.compile(r"Using default MoE config\. Performance might be sub-optimal! Config file not found at (.+)$")
BACKEND = re.compile(r"Using ([A-Z0-9_]+) [A-Za-z0-9]+ MoE (?:LoRA )?backend")


def read_engine_log(text: str) -> dict:
    """internal/kerneltuning ReadEngineLog, line for line."""
    reading: dict = {}
    for line in ANSI.sub("", text).split("\n"):
        line = line.rstrip("\r ")
        if (match := USED.search(line)) and match.group(1) not in reading.setdefault("config_files", []):
            reading["config_files"].append(match.group(1))
        if match := DEFAULT.search(line):
            for missing in match.group(1).split(", "):
                name = os.path.basename(missing.strip())
                if name and name not in reading.setdefault("default_for", []):
                    reading["default_for"].append(name)
        if (match := BACKEND.search(line)) and not reading.get("backend"):
            reading["backend"] = match.group(1)
    return {key: value for key, value in reading.items() if value}


def read_from(reading: dict, folder: str) -> list[str]:
    return [os.path.basename(path) for path in reading.get("config_files", []) if os.path.dirname(os.path.normpath(path)) == os.path.normpath(folder)]


def invoked(reading: dict, arm: str, files: list[str]) -> str:
    """kerneltuning proof's rule: "" when the run served what its arm says."""
    if reading.get("backend") and reading["backend"] != "TRITON":
        return f"the engine chose the {reading['backend']} MoE backend, which reads no tuned file"
    read = read_from(reading, tune_moe.MOUNT_PATH)
    for name in files:
        if arm == "candidate" and (name not in read or name in reading.get("default_for", [])):
            return f"a tuned run's engine did not read {name}"
        if arm == "control" and (name in read or name not in reading.get("default_for", [])):
            return f"a control run's engine did not report its default for {name}"
    return ""


# ----------------------------------------------------------------------------- host and engine

def host_facts(dry: bool) -> dict:
    if dry:
        return {"gpu_count": 0, "name": "stand-in", "driver_version": "stand-in"}
    fields = "name,uuid,pci.bus_id,driver_version,vbios_version,memory.total,power.limit,clocks.max.sm"
    facts: dict = {}
    probe = subprocess.run(["nvidia-smi", f"--query-gpu={fields}", "--format=csv,noheader"], capture_output=True, text=True, timeout=30)
    rows = probe.stdout.strip().splitlines() if probe.returncode == 0 else []
    facts["gpu_count"] = len(rows)
    if rows:
        facts.update(zip(fields.split(","), (part.strip() for part in rows[0].split(","))))
    for name in ("MODAL_REGION", "MODAL_CLOUD_PROVIDER", "MODAL_TASK_ID"):
        facts[name.lower()] = os.environ.get(name, "unknown")
    return facts


class GpuSampler:
    """Clock, power, temperature and use of the GPU while a cell runs (the map's)."""

    QUERY = "clocks.sm,power.draw,temperature.gpu,utilization.gpu,memory.used"

    def __init__(self, enabled: bool) -> None:
        self.enabled, self.process = enabled, None

    def start(self) -> None:
        if self.enabled:
            self.process = subprocess.Popen(["nvidia-smi", f"--query-gpu={self.QUERY}", "--format=csv,noheader,nounits", "-lms", "1000"],
                                            stdout=subprocess.PIPE, stderr=subprocess.DEVNULL, text=True)

    def stop(self) -> dict:
        if self.process is None:
            return {}
        self.process.terminate()
        try:
            samples, _ = self.process.communicate(timeout=10)
        except subprocess.TimeoutExpired:
            self.process.kill()
            return {}
        rows = []
        for line in samples.strip().splitlines():
            try:
                rows.append([float(part) for part in line.split(",")])
            except ValueError:
                continue
        if not rows:
            return {}
        columns = list(zip(*rows))
        mean = lambda values: round(sum(values) / len(values), 1)  # noqa: E731
        return {"samples": len(rows), "sm_clock_mhz_mean": mean(columns[0]), "power_w_mean": mean(columns[1]), "temperature_c_max": max(columns[2]),
                "utilization_pct_mean": mean(columns[3]), "memory_used_mib_max": max(columns[4])}


def engine_env(extra: dict) -> dict:
    """The container's environment without the Modal function's own Python paths."""
    env = dict(os.environ)
    kept = [part for part in env.get("PYTHONPATH", "").split(":") if part and not part.startswith(("/pkg", "/root", "/__modal"))]
    env.pop("PYTHONPATH", None)
    env.pop("PYTHONHOME", None)
    env.pop(TUNED_FOLDER_VARIABLE, None)
    if kept:
        env["PYTHONPATH"] = ":".join(kept)
    env.update(extra)
    return env


def engine_vllm() -> str:
    found = shutil.which("vllm", path="/opt/venv/bin:/usr/local/bin:/usr/bin:/bin")
    if not found:
        raise RuntimeError("the image has no `vllm` executable")
    return found


def engine_python() -> str:
    """The interpreter the image's `vllm` runs on, from its first line."""
    first = Path(engine_vllm()).read_text(errors="replace").splitlines()[0]
    if first.startswith("#!") and Path(first[2:].strip().split()[0]).exists():
        return first[2:].strip().split()[0]
    for python in ("/opt/venv/bin/python", "/usr/bin/python3"):
        if Path(python).exists():
            return python
    raise RuntimeError("no Python that runs the image's vllm")


def server_command(dry: bool) -> list[str]:
    if dry:
        return [sys.executable, str(Path(__file__).with_name("stand_in_server.py")), str(PORT), MODEL["id"]]
    return [engine_vllm(), "serve", MODEL["dir"], "--served-model-name", MODEL["id"], "--host", "127.0.0.1", "--port", str(PORT), *SERVE_ARGS]


def start_server(dry: bool, tuned_folder: str | None, log_path: Path, stand_in_file: str) -> subprocess.Popen:
    extra = dict(SERVE_ENV)
    if tuned_folder:
        extra[TUNED_FOLDER_VARIABLE] = tuned_folder
    if dry:
        extra["STAND_IN_MOE_FILE"], extra["STAND_IN_REPORTED_FOLDER"] = stand_in_file, tune_moe.MOUNT_PATH
    log_path.write_text("")
    with log_path.open("a") as log:
        return subprocess.Popen(server_command(dry), stdout=log, stderr=subprocess.STDOUT, env=engine_env(extra), start_new_session=True)


def stop_server(process: subprocess.Popen) -> None:
    if process.poll() is not None:
        return
    for sig, wait in ((signal.SIGTERM, 30), (signal.SIGKILL, 10)):
        try:
            os.killpg(process.pid, sig)
        except (ProcessLookupError, PermissionError):
            process.send_signal(sig)
        try:
            process.wait(timeout=wait)
            return
        except subprocess.TimeoutExpired:
            continue


def wait_gpu_free(dry: bool, seconds: float = 90.0) -> None:
    """A stopped engine's memory must be back before the next one profiles the GPU."""
    if dry:
        return
    deadline = time.monotonic() + seconds
    while time.monotonic() < deadline:
        probe = subprocess.run(["nvidia-smi", "--query-gpu=memory.used", "--format=csv,noheader,nounits"], capture_output=True, text=True, timeout=30)
        try:
            if float(probe.stdout.strip().splitlines()[0]) < 2048:
                return
        except (ValueError, IndexError):
            pass
        time.sleep(2)
    raise RuntimeError("the GPU's memory was not freed after the engine stopped")


def healthy(target: loadgen.Target) -> bool:
    import http.client
    try:
        connection = http.client.HTTPConnection(target.host, target.port, timeout=3)
        connection.request("GET", "/health")
        return connection.getresponse().status == 200
    except OSError:
        return False


def wait_ready(process: subprocess.Popen, target: loadgen.Target, log_path: Path, seconds: float, log) -> float:
    started, note = time.monotonic(), 0.0
    while not healthy(target):
        if process.poll() is not None:
            raise RuntimeError(f"server exited with {process.returncode} while starting: {log_path.read_text(errors='replace')[-1500:]}")
        waited = time.monotonic() - started
        if waited > seconds:
            raise TimeoutError(f"server not ready after {seconds:.0f} s")
        if waited - note >= 30:
            note = waited
            tail = log_path.read_text(errors="replace").strip().splitlines()[-1:]
            log(f"starting, {waited:.0f} s: {(tail[0][-160:] if tail else '')}")
        time.sleep(1 if waited < 10 else 2)
    # One short answer before measuring: the router-free path still warms the first request.
    record = loadgen.stream_request(target, loadgen.chat_body(target.model, "Say OK.", 4))
    if not record["ok"]:
        raise RuntimeError(f"the server's first answer failed: {record['error']}")
    return round(time.monotonic() - started, 1)


# ----------------------------------------------------------------------------- inputs

def dig(document: dict, path: tuple) -> list:
    for part in path:
        document = document[part]
    return document


def load_inputs(dry: bool) -> tuple[dict, list[dict], dict]:
    """The cells' prompts and the task set, each file checked against its manifest."""
    facts, files = {}, {}
    for key, (path, sha256) in PROMPT_FILES.items():
        if dry and not Path(path).exists():
            files[key], facts[key] = stand_in_files()[key], {"source": "stand-in (dry run)"}
            continue
        data = Path(path).read_bytes()
        digest = hashlib.sha256(data).hexdigest()
        if digest != sha256:
            raise RuntimeError(f"{path} has sha256 {digest}, its manifest says {sha256}")
        files[key], facts[key] = json.loads(data), {"source": path, "sha256": digest, "bytes": len(data)}
    prompts = {cell: dig(files[key], path) for cell, key, path, _ in CELLS}
    items = files["quality"]["items"]
    # Prompts asked one at a time, so batching cannot change an answer: set v1's short
    # "answers" slice, which no measured level reads.
    sequential = [{"id": prompt["id"], "prompt": prompt["content"], "max_tokens": MAX_TOKENS}
                  for prompt in dig(files["v1"], ("shapes", "short", "answers"))[:PLAN["sequential_answers"]]]
    # Each cell's traffic shape without its text: the prompts' token counts in order.
    facts["shape_digests"] = {cell: "sha256:" + hashlib.sha256(json.dumps([prompt.get("tokens") for prompt in prompts[cell]]).encode()).hexdigest()
                              for cell in prompts}
    return prompts, items, sequential, facts


def stand_in_files() -> dict:
    words = ["alpha", "bravo", "charlie", "delta", "echo", "foxtrot"]
    short = [{"id": f"stand-in:{index}", "content": " ".join(words[index % 6:] + words[:index % 6]), "tokens": 6} for index in range(400)]
    items = [{"id": f"stand-in:task:{index}", "source": "stand-in", "kind": "number", "gold": str(index % 7), "max_tokens": 32,
              "prompt": f"What is {index} modulo 7? End with Answer: <number>."} for index in range(40)]
    return {"v1": {"shapes": {"short": {"levels": {"32": short}, "answers": short[:24]}}}, "v2": {"kinds": {"short": {"levels": {"128": short}}}},
            "quality": {"items": items}}


def model_config(dry: bool) -> dict:
    if dry:
        # The plan's model's MoE fields (its config.json at the pinned revision).
        return {"text_config": dict(PLAN["dry_moe"], num_experts_per_tok=8, hidden_size=2048),
                "quantization_config": {"quant_method": "fp8", "weight_block_size": [128, 128]}}
    marker = Path(MODEL["dir"]) / ".complete"
    if not marker.exists() or marker.read_text().strip() != MODEL["revision"]:
        raise RuntimeError(f"the volume does not hold {MODEL['id']} at {MODEL['revision']}; run the download action first")
    return json.loads((Path(MODEL["dir"]) / "config.json").read_text())


# ----------------------------------------------------------------------------- the machine

def gap_check(dry: bool, out_dir: Path, deadline: float, log) -> dict:
    """One unmeasured start of the untuned engine: which MoE backend it chose and which
    tuned file it looked for and did not find. Tuning targets exactly that file; a start
    that found a shipped file for the shape, or chose another backend, ends the machine
    before any tuning. The start also compiles, so the window's first start is warm."""
    log_path = out_dir / "gap-check.server.log"
    process = start_server(dry, None, log_path, stand_in_file=tune_moe.file_name(tune_moe.shape_from_config(model_config(dry)), PLAN["device"]))
    try:
        startup = wait_ready(process, loadgen.Target("127.0.0.1", PORT, MODEL["id"]), log_path, min(STARTUP_SECONDS, deadline - time.monotonic()), log)
    finally:
        stop_server(process)
    wait_gpu_free(dry)
    reading = read_engine_log(log_path.read_text(errors="replace"))
    return {"startup_seconds": startup, "engine": reading}


def tune_step(dry: bool, out_dir: Path, budget_seconds: float, file_name: str, log) -> tuple[dict, dict]:
    """Runs the tuner with the engine's own Python on this GPU for the file the engine
    looked for, and returns the set and its report."""
    config = model_config(dry)
    target = out_dir / "tuning"
    if dry:
        base = tune_moe.shape_from_config(config)
        shape, device = tune_moe.shape_from_file_name(file_name, base.hidden, base.topk)
        result = tune_moe.tune(shape, device, RUNTIME, VERSION, LOADS, tune_moe.Bench(shape, True, tune_moe.PEAK_BANDWIDTH[device]), budget_seconds,
                               TUNE_PER_KEY_SECONDS, log=log)
        kernel_set = tune_moe.write(result, shape, device, RUNTIME, VERSION, str(target))
        return kernel_set, result["report"]
    config_path = out_dir / "model-config.json"
    config_path.write_text(json.dumps(config))
    command = [engine_python(), str(Path(tune_moe.__file__)), "--model-config", str(config_path), "--file-name", file_name, "--loads", json.dumps(LOADS),
               "--runtime", RUNTIME, "--version", VERSION, "--budget-seconds", str(budget_seconds), "--per-key-seconds", str(TUNE_PER_KEY_SECONDS),
               "--out", str(target)]
    env = engine_env({})
    done = subprocess.run(command, capture_output=True, text=True, env=env, timeout=budget_seconds * 1.4 + 300)
    (out_dir / "tuning.log").write_text(done.stdout + done.stderr)
    if done.returncode != 0:
        raise RuntimeError(f"the tuner failed: {(done.stdout + done.stderr)[-2000:]}")
    for line in done.stdout.splitlines()[-40:]:
        log(line)
    return json.loads((target / "kernel-configs.json").read_text()), json.loads((target / "tuning-report.json").read_text())


def install_set(kernel_set: dict, folder: str) -> list[str]:
    """Writes the set's files where a candidate's engine reads them, checking each digest."""
    shutil.rmtree(folder, ignore_errors=True)
    os.makedirs(folder)
    names = []
    for entry in kernel_set["files"]:
        content = entry["content"].encode()
        if hashlib.sha256(content).hexdigest() != entry["sha256"] or "/" in entry["name"]:
            raise RuntimeError(f"tuned file {entry['name']} does not match its sha256")
        Path(folder, entry["name"]).write_bytes(content)
        names.append(entry["name"])
    if tune_moe.set_digest(kernel_set) != kernel_set["digest"]:
        raise RuntimeError("the tuned set does not match its digest")
    return sorted(names)


def run_window(window: str, kernel_set: dict, dry: bool, out_dir: Path, deadline: float, log,
               warmup: float = WARMUP_SECONDS, span: float = WINDOW_SECONDS) -> tuple[dict, dict]:
    """Serves control and candidate in turn on this GPU and measures every cell each time."""
    folder = tune_moe.MOUNT_PATH if not dry else str(out_dir / "mount" / "kernel-configs")
    files = install_set(kernel_set, folder)
    prompts, items, sequential_items, input_facts = load_inputs(dry)
    if dry:
        items = items[:40]
    target = loadgen.Target("127.0.0.1", PORT, MODEL["id"])
    record = {"schema": WINDOW_SCHEMA, "window": window, "machine": {}, "runtime": RUNTIME, "runtime_version": VERSION,
              "set_digest": kernel_set["digest"], "files": files, "cells": [cell for cell, *_ in CELLS], "runs": []}
    detail = {"schema": DETAIL_SCHEMA, "window": window, "dry": dry, "inputs": input_facts, "model": MODEL, "serve_args": SERVE_ARGS,
              "serve_env": SERVE_ENV, "warmup_seconds": warmup, "window_seconds": span, "runs": [], "stopped": None}
    quality: dict = {"items": [{"id": item["id"], "gold": str(item["gold"])} for item in items], "answers": {}}
    sequential: dict = {"prompts": [item["id"] for item in sequential_items], "answers": {}}
    run_seconds: list[float] = []
    for index in range(2 * RUNS_PER_ARM + 1):
        arm = "candidate" if index % 2 else "control"
        needed = max(run_seconds) * 1.15 if run_seconds else 0
        if run_seconds and time.monotonic() + needed > deadline:
            detail["stopped"] = f"out of time before run {index + 1}: a run takes about {needed:.0f} s"
            break
        began = time.monotonic()
        log_path = out_dir / f"{window}-run{index + 1}-{arm}.server.log"
        process = start_server(dry, folder if arm == "candidate" else None, log_path, files[0])
        run = {"arm": arm, "engine": {}, "cells": {}}
        extra = {"arm": arm, "cells": {}}
        try:
            extra["startup_seconds"] = wait_ready(process, target, log_path, min(STARTUP_SECONDS, deadline - time.monotonic()), log)
            run["engine"] = read_engine_log(log_path.read_text(errors="replace"))
            reason = invoked(run["engine"], arm, files)
            if reason:
                detail["stopped"] = f"run {index + 1}: {reason}"
            else:
                for cell, _, _, at_once in CELLS:
                    sampler = GpuSampler(not dry)
                    sampler.start()
                    summary = loadgen.run_level(target, prompts[cell], at_once, warmup, span, MAX_TOKENS, COOLDOWN_SECONDS)
                    summary["gpu"] = sampler.stop()
                    extra["cells"][cell] = summary
                    run["cells"][cell] = {key: summary[key] for key in ("requests", "failed", "output_token_throughput", "ttft_p95_ms", "tpot_p95_ms", "latency_p95_ms")
                                          if summary.get(key) is not None}
                    log(f"{window} run {index + 1} {arm} {cell}: {summary['output_token_throughput']} tok/s, {summary['failed']} of {summary['requests']} failed")
                    if summary["failed"]:
                        detail["stopped"] = f"run {index + 1} {cell}: {summary['failed']} requests failed ({summary['error_examples'][:1]})"
                        break
                if not detail["stopped"] and window == "first" and index in QUALITY_RUNS:
                    started = time.monotonic()
                    quality["answers"][QUALITY_RUNS[index]] = loadgen.answer_all(target, items, QUALITY_AT_ONCE)
                    extra["quality_seconds"] = round(time.monotonic() - started, 1)
                    if sequential_items:
                        sequential["answers"][QUALITY_RUNS[index]] = loadgen.answer_all(target, sequential_items, 1)
            if process.poll() is not None:
                raise RuntimeError(f"server exited with {process.returncode} while serving")
        except Exception as failure:  # noqa: BLE001 - recorded, never retried
            detail["stopped"] = f"run {index + 1}: {type(failure).__name__}: {str(failure)[-1500:]}"
        finally:
            stop_server(process)
        extra["seconds"] = round(time.monotonic() - began, 1)
        record["runs"].append(run)
        detail["runs"].append(extra)
        run_seconds.append(extra["seconds"])
        if detail["stopped"]:
            break
        wait_gpu_free(dry)
    if window == "first":
        record["quality"] = quality
        if sequential_items:
            record["sequential"] = sequential
    record["reproduction"] = {
        "command": os.environ.get("KERNEL_TUNE_COMMAND", ""), "commit": os.environ.get("KERNEL_TUNE_COMMIT", ""),
        "image_digest": os.environ.get("KERNEL_TUNE_IMAGE_DIGEST", ""),
        "traffic_shape": "; ".join(f"{cell}: {key} {'.'.join(path)} ({PROMPT_FILES[key][1][:12]})" for cell, key, path, _ in CELLS),
        "shape_digests": input_facts["shape_digests"], "weights": PLAN.get("weights", ""), "kv_cache": PLAN.get("kv_cache", "")}
    return record, detail


SHIPPED_CHECK = r'''
import json, os, sys
from vllm.model_executor.layers.fused_moe import fused_moe
folder = os.path.join(os.path.dirname(os.path.realpath(fused_moe.__file__)), "configs")
names = [fused_moe.get_config_file_name(int(e), int(n), "fp8_w8a8", [128, 128]) for e, n in (pair.split(",") for pair in sys.argv[1:])]
print(json.dumps({name: os.path.exists(os.path.join(folder, name)) for name in names}))
'''

# Rung 2's pass bar (rules-moe-shapes-l40s-2026-11-26-v3.md): rung 3 is asked for only when the
# tuned file projects at least this much end to end for rung 3's model at one of its loads,
# from rung 1's share and the kernel gain the tuner kept at the load's step size.
PROJECTED_GATE = 1.02


def shapes_verdict(plan: dict, results: list[dict]) -> dict:
    """Projected end-to-end speedups per shape and load, and whether rung 3 is worth asking.
    A shape whose tuning errored, and a load whose step size has no measured engine time,
    read "unknown", never a zero gain."""
    import amdahl
    rows, rung3, unknown = [], False, []
    for shape, result in zip(plan["shapes"], results):
        report = result.get("report")
        if result.get("error") or result.get("stopped") or not report:
            reason = (result.get("error") or result.get("stopped") or "no tuning report")[-300:]
            unknown.append(f"{shape['model']}: {reason}")
            for load in plan["loads"]:
                rows.append({"model": shape["model"], "running": load["running"], "status": "unknown", "reason": reason, "projected_end_to_end": None})
            continue
        keys = {int(key): row for key, row in report.get("keys", {}).items()}
        for load in plan["loads"]:
            running = load["running"]
            key = min(keys, key=lambda k: abs(k - running)) if keys else None
            row = keys.get(key, {})
            share = shape["share"][running]
            entry = {"model": shape["model"], "running": running, "step_size": key, "kept": row.get("kept"), "share_estimate": share}
            if row.get("engine_error") or not row.get("engine_us"):
                entry.update(status="unknown", reason="the engine's own setting did not run at this step size, so no gain can be computed",
                             engine_error=row.get("engine_error"), projected_end_to_end=None)
                unknown.append(f"{shape['model']} at {running}: no engine time at step size {key}")
            else:
                gain = (row["engine_us"] / row["best_us"] - 1) if row.get("kept") == "tuned" and row.get("best_us") else 0.0
                projected = amdahl.amdahl(share, 1 + gain)
                entry.update(status="measured", kernel_gain=round(gain, 4), projected_end_to_end=round(projected, 4), planner_gate_1_05=projected >= 1.05)
                if shape.get("rung3") and projected >= PROJECTED_GATE:
                    rung3 = True
            rows.append(entry)
        if report.get("engine_fails_at"):
            unknown.append(f"{shape['model']}: the engine's own setting does not run at step sizes {report['engine_fails_at']} on this GPU")
    rung3_models = {shape["model"] for shape in plan["shapes"] if shape.get("rung3")}
    rung3_errored = any(row["status"] == "unknown" for row in rows if row["model"] in rung3_models)
    ask = rung3 and not any(result.get("error") or not result.get("report") for shape, result in zip(plan["shapes"], results) if shape.get("rung3"))
    summary = ("ask for rung 3" if ask else "unknown: " + "; ".join(unknown[:3]) if rung3_errored else
               f"below {PROJECTED_GATE}x for rung 3's model at its loads: the ladder stops for it on this GPU")
    return {"rows": rows, "projected_gate": PROJECTED_GATE, "ask_for_rung3": ask, "summary": summary, "unknown": unknown}


def shapes_machine(dry: bool, out_dir: str, limit_seconds: float, log=print) -> dict:
    """Rung 2: tune each plan shape on this GPU from the shapes alone. Never raises."""
    started = time.monotonic()
    out = Path(out_dir)
    out.mkdir(parents=True, exist_ok=True)
    result: dict = {"plan": PLAN_ID, "dry": dry, "started_epoch": time.time(), "shapes": []}
    try:
        result["facts"] = host_facts(dry)
        if not dry and (result["facts"].get("gpu_count") != 1 or EXPECT_GPU not in str(result["facts"].get("name"))):
            raise RuntimeError(f"this run needs one {EXPECT_GPU}, the host has {result['facts'].get('gpu_count')} x {result['facts'].get('name')}")
        device = PLAN["device"] if dry else tune_moe.engine_device_name(result["facts"].get("name", ""))
        pairs = [f"{shape['experts']},{shape['intermediate']}" for shape in PLAN["shapes"]]
        if dry:
            shipped = {tune_moe.file_name(tune_moe.Shape(int(p.split(',')[0]), int(p.split(',')[1]), "fp8_w8a8", 128, 128, 2048, 8), device): False for p in pairs}
        else:
            done = subprocess.run([engine_python(), "-c", SHIPPED_CHECK, *pairs], capture_output=True, text=True, env=engine_env({}), timeout=600)
            if done.returncode != 0:
                raise RuntimeError(f"the shipped-file check failed: {(done.stdout + done.stderr)[-1500:]}")
            shipped = json.loads(done.stdout.strip().splitlines()[-1])
        result["shipped"] = shipped
        budget = PLAN["tune_budget_seconds"] if not dry else 20.0
        for index, spec in enumerate(PLAN["shapes"]):
            if time.monotonic() - started > limit_seconds - budget * 1.3 - 120:
                result["shapes"].append({"model": spec["model"], "stopped": "out of time before this shape"})
                continue
            shape = tune_moe.Shape(spec["experts"], spec["intermediate"], "fp8_w8a8", 128, 128, spec["hidden"], spec["topk"])
            target = out / f"shape-{index + 1}"
            row = {"model": spec["model"], "file": tune_moe.file_name(shape, device)}
            row["shipped_for_this_gpu"] = shipped.get(row["file"])
            began = time.monotonic()
            sampler = GpuSampler(not dry)  # clocks, power and temperature while tuning: logged, not locked
            sampler.start()
            if dry:
                tuned = tune_moe.tune(shape, device, RUNTIME, VERSION, PLAN["loads"], tune_moe.Bench(shape, True, tune_moe.PEAK_BANDWIDTH[device]), budget, 10.0, log=log)
                row["set"] = tune_moe.write(tuned, shape, device, RUNTIME, VERSION, str(target))
                row["report"] = tuned["report"]
            else:
                command = [engine_python(), str(Path(tune_moe.__file__)), "--shape", f"{shape.experts},{shape.intermediate},{shape.hidden},{shape.topk}",
                           "--loads", json.dumps(PLAN["loads"]), "--runtime", RUNTIME, "--version", VERSION, "--budget-seconds", str(budget),
                           "--per-key-seconds", str(TUNE_PER_KEY_SECONDS), "--out", str(target)]
                done = subprocess.run(command, capture_output=True, text=True, env=engine_env({}), timeout=budget * 1.4 + 300)
                (target.parent / f"shape-{index + 1}.log").write_text(done.stdout + done.stderr)
                if done.returncode != 0:
                    row["error"] = (done.stdout + done.stderr)[-1500:]
                else:
                    row["set"] = json.loads((target / "kernel-configs.json").read_text())
                    row["report"] = json.loads((target / "tuning-report.json").read_text())
            row["gpu"] = sampler.stop()
            row["seconds"] = round(time.monotonic() - began, 1)
            log(f"{spec['model']}: kept {row.get('report', {}).get('kept_tuned')} in {row['seconds']} s")
            result["shapes"].append(row)
        result["verdict"] = shapes_verdict(PLAN, result["shapes"])
    except Exception as failure:  # noqa: BLE001 - recorded in the result
        result["error"] = f"{type(failure).__name__}: {str(failure)[-3000:]}"
        log(result["error"][:600])
    finally:
        result["container_seconds"] = round(time.monotonic() - started, 1)
        (out / "shapes-result.json").write_text(json.dumps(result, indent=1) + "\n")
    return result


def free_port() -> int:
    """A port no other process holds: a laptop running a dry run is shared with other work."""
    import socket
    with socket.socket() as probe:
        probe.bind(("127.0.0.1", 0))
        return probe.getsockname()[1]


def machine(window: str, kernel_set: dict | None, dry: bool, out_dir: str, limit_seconds: float, log=print) -> dict:
    """One container's whole life. Never raises: a failure is part of the result."""
    global PORT
    if dry:
        PORT = free_port()
    started = time.monotonic()
    deadline = started + limit_seconds - 60
    out = Path(out_dir)
    out.mkdir(parents=True, exist_ok=True)
    result: dict = {"window": window, "dry": dry, "started_epoch": time.time()}
    try:
        result["facts"] = host_facts(dry)
        log(json.dumps(result["facts"]))
        if not dry and (result["facts"].get("gpu_count") != 1 or EXPECT_GPU not in str(result["facts"].get("name"))):
            raise RuntimeError(f"this run needs one {EXPECT_GPU}, the host has {result['facts'].get('gpu_count')} x {result['facts'].get('name')}")
        device = tune_moe.engine_device_name(result["facts"].get("name", "")) if not dry else PLAN["device"]
        if window == "first":
            result["gap_check"] = gap_check(dry, out, deadline, log)
            engine = result["gap_check"]["engine"]
            log(f"gap check: {json.dumps(result['gap_check'])}")
            gaps = [name for name in engine.get("default_for", []) if f"device_name={device}," in name or name.endswith(f"device_name={device}.json")]
            if engine.get("backend") != "TRITON":
                result["stopped"] = f"the engine chose the {engine.get('backend') or 'unreported'} MoE backend, which reads no tuned file: nothing to tune"
                return result
            if len(gaps) != 1:
                result["stopped"] = (f"the engine looked for {len(gaps)} fused-MoE files it did not find on {device} ({engine.get('default_for')}); "
                                     f"it read {engine.get('config_files')}. The rules tune exactly one: nothing measured")
                return result
            if kernel_set is not None:
                # A set tuned earlier on this GPU type (rung 2) is measured as given: it must be
                # exactly the file this engine looks for.
                names = [entry["name"] for entry in kernel_set.get("files", [])]
                if names != gaps:
                    result["stopped"] = f"the given set holds {names}, the engine looks for {gaps}: nothing measured"
                    return result
                result["kernel_set"], result["tuning_report"] = kernel_set, None
            else:
                tune_started = time.monotonic()
                kernel_set, result["tuning_report"] = tune_step(dry, out, TUNE_BUDGET_SECONDS if not dry else 30.0, gaps[0], log)
                result["tuning_seconds"] = round(time.monotonic() - tune_started, 1)
                result["kernel_set"] = kernel_set
                if not result["tuning_report"].get("kept_tuned"):
                    result["stopped"] = "the tuner kept the engine's own setting at every step size: nothing to measure"
                    return result
        if kernel_set is None or kernel_set.get("device") != device:
            raise RuntimeError(f"the tuned set is for {None if kernel_set is None else kernel_set.get('device')}, this GPU is {device}")
        warmup, span = (2.0, 8.0) if dry else (WARMUP_SECONDS, WINDOW_SECONDS)
        record, detail = run_window(window, kernel_set, dry, out, deadline, log, warmup, span)
        record["machine"] = {"gpu": str(result["facts"].get("name", "")), "driver": str(result["facts"].get("driver_version", "")),
                             "host": str(result["facts"].get("modal_task_id", ""))}
        result["record"], result["detail"] = record, detail
        (out / f"window-{window}.json").write_text(json.dumps(record, indent=1) + "\n")
        (out / f"window-{window}-detail.json").write_text(json.dumps(detail, indent=1) + "\n")
        if detail["stopped"]:
            result["stopped"] = detail["stopped"]
    except Exception as failure:  # noqa: BLE001 - recorded in the result
        result["error"] = f"{type(failure).__name__}: {str(failure)[-3000:]}"
        log(result["error"][:600])
    finally:
        result["container_seconds"] = round(time.monotonic() - started, 1)
        (out / f"machine-{window}.json").write_text(json.dumps(result, indent=1) + "\n")
    return result
