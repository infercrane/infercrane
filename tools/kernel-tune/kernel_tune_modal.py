"""Kernel-tuning proof on Modal (development only): tune vLLM 0.34.0's fused-MoE kernel for
Qwen3.6-35B-A3B-FP8 on one H100 at the map's short-chat loads, then judge the tuned set end
to end against the untuned engine on that machine and again on a fresh one.

    python3 tools/kernel-tune/kernel_tune_modal.py dry                    # $0: the whole path on this laptop, stand-ins only
    python3 tools/kernel-tune/kernel_tune_modal.py smoke                  # CPU: both images start in Modal and import this file (cents)
    python3 tools/kernel-tune/kernel_tune_modal.py download               # CPU: stage the pinned weights (cents, 30 min at most)
    python3 tools/kernel-tune/kernel_tune_modal.py preflight              # CPU: read the image's facts (cents, 10 min at most)
    python3 tools/kernel-tune/kernel_tune_modal.py run --budget-usd 10    # GPU: first machine, then a fresh one if the first gained

`smoke` comes first: the laptop dry run cannot show a container failing at startup. `run`
refuses a budget below the two machines' caps at list prices. Every remote call is
cancelled from here at its own wall-clock limits: when its container has not started in
time (CPU 5 min, GPU 20 min), at the first startup crash Modal reports, at its limit after
it started (a GPU machine's cap), and in any case before this process returns; the function
timeout is the backstop. The fresh machine runs only when the first window showed a gain
the judge calls improved and nothing it rejects. The verdict is
`infercrane optimize evidence kernel-tuning`'s: the campaign judge's code.

Nothing here creates, reads or prints a Modal credential.
"""

from __future__ import annotations

import argparse
import json
import os
import subprocess
import sys
import time
from pathlib import Path

import modal

HERE = Path(__file__).resolve().parent
sys.path.insert(0, str(HERE))
import proof  # noqa: E402
import tune_moe  # noqa: E402

APP_NAME = "infercrane-kernel-tune"
# vllm/vllm-openai v0.34.0 by digest (tools/poc-modal/models.py, read 2026-11-23).
VLLM_IMAGE = "vllm/vllm-openai@sha256:20fdb089454afbb324efa4110a2c025ff7b27cc1a086e07aa580b18df8ad1ce1"
PLAN = proof.PLAN  # KERNEL_TUNE_PLAN picks it; the container resolves the same one
SHAPES_ONLY = PLAN.get("kind") == "shapes"
GPU_REQUEST = PLAN["gpu"]  # "H100!" stops Modal from handing out a larger GPU for an H100 request
GPU_CPU, GPU_MEMORY_MIB = (4.0, 16384) if SHAPES_ONLY else (8.0, 49152)
# Modal list prices per hour. H100 and the CPU and memory rates as tools/fast-track-map uses
# them. L40S: ASSUMED $1.95/h, because modal.com did not resolve from this machine on 26 Nov;
# whoever launches confirms it on Modal's pricing or Usage page, since the dollar caps scale
# with it (the wall-clock caps do not).
GPU_PER_HOUR = {"H100!": 3.9492, "L40S": 1.95}
GPU_PER_SECOND, CORE_PER_SECOND, GIB_PER_SECOND = GPU_PER_HOUR[GPU_REQUEST] / 3600, 0.0000131, 0.00000222
LIMITS = PLAN["limits"]  # each machine's cap after it starts
CANCEL_MARGIN_SECONDS = 60
QUEUE_SECONDS = 20 * 60  # waiting for a GPU is not billed; a startup crash is caught sooner (startup_crashed)
ROOT = HERE.parents[1] if len(HERE.parents) > 1 else HERE

app = modal.App(APP_NAME)
weights = modal.Volume.from_name("infercrane-poc-modal-weights", create_if_missing=True)
prompts = modal.Volume.from_name("infercrane-fast-track-map", create_if_missing=True)
results = modal.Volume.from_name("infercrane-kernel-tune", create_if_missing=True)
status = modal.Dict.from_name("infercrane-kernel-tune-status", create_if_missing=True)
def _commit() -> str:
    try:
        return subprocess.run(["git", "-C", str(HERE), "rev-parse", "HEAD"], capture_output=True, text=True, timeout=10).stdout.strip()
    except (OSError, subprocess.TimeoutExpired):
        return ""


# What a result needs to be reproduced, read on the laptop and carried into the container.
REPRODUCTION = {"KERNEL_TUNE_PLAN": proof.PLAN_ID, "KERNEL_TUNE_COMMIT": os.environ.get("KERNEL_TUNE_COMMIT") or _commit(),
                "KERNEL_TUNE_IMAGE_DIGEST": VLLM_IMAGE.split("@", 1)[1],
                "KERNEL_TUNE_COMMAND": os.environ.get("KERNEL_TUNE_COMMAND") or f"KERNEL_TUNE_PLAN={proof.PLAN_ID} python3 tools/kernel-tune/kernel_tune_modal.py " + " ".join(sys.argv[1:])}
os.environ.update(REPRODUCTION)
# Every container imports this file, so every image carries the local modules it imports.
# 8f579b02's download image did not: its container crashed at startup 14 times. The
# reproduction values go in as each function's environment, not as an image layer, so the
# smoke check and the paid run start the same image.
LOCAL_MODULES = ("proof", "tune_moe", "loadgen", "amdahl")
image = modal.Image.from_registry(VLLM_IMAGE, add_python="3.12").entrypoint([]).add_local_python_source(*LOCAL_MODULES)
download_image = (modal.Image.debian_slim(python_version="3.12").pip_install("huggingface_hub[hf_xet]==1.1.4")
                  .add_local_python_source(*LOCAL_MODULES))
VOLUMES = {"/weights": weights, "/fasttrack": prompts, "/results": results}
# Each CPU call's own wall-clock limits from here: (to start, to run after it started). A
# container that crashes at startup never starts, so it is cancelled at the first one.
CPU_CALLS = {"smoke": (5 * 60, 3 * 60), "download": (5 * 60, 25 * 60), "preflight": (5 * 60, 5 * 60)}
CPU_OPTIONS = dict(env=REPRODUCTION, retries=0, single_use_containers=True, max_containers=1)


def rate_per_second() -> float:
    return GPU_PER_SECOND + GPU_CPU * CORE_PER_SECOND + GPU_MEMORY_MIB / 1024 * GIB_PER_SECOND


def cap_usd(windows: tuple[str, ...] = ("shapes",) if SHAPES_ONLY else ("first", "fresh")) -> float:
    return round(sum(LIMITS[window] + CANCEL_MARGIN_SECONDS for window in windows) * rate_per_second(), 2)


ENGINE_SMOKE = r'''
import importlib.metadata, json
import torch, triton, vllm
from vllm.model_executor.layers.fused_moe import fused_moe
print(json.dumps({name: importlib.metadata.version(name) for name in ("vllm", "triton", "torch")}))
'''


# The engine's own override_config, without a GPU: whether a setting that raises stays in
# force (vLLM 0.34.0's does), and whether the tuner's `setting` puts it back.
OVERRIDE_SMOKE = r'''
import json, sys
sys.path.insert(0, sys.argv[1])
import tune_moe
import vllm.model_executor.layers.fused_moe as fused_moe
leaks = False
try:
    with fused_moe.override_config({"BLOCK_SIZE_N": 256}):
        raise RuntimeError("raised inside the override")
except RuntimeError:
    leaks = fused_moe.get_config() is not None
fused_moe._config = None
restored = False
try:
    with tune_moe.setting({"BLOCK_SIZE_N": 256}):
        raise RuntimeError("raised inside the setting")
except RuntimeError:
    restored = fused_moe.get_config() is None
print(json.dumps({"vllm_override_leaks": leaks, "setting_restores": restored}))
'''


@app.function(image=download_image, volumes={"/weights": weights}, cpu=0.25, memory=1024, timeout=CPU_CALLS["smoke"][1], **CPU_OPTIONS)
def smoke_download(tag: str) -> dict:
    """The download container's start, for cents: it imports this file as `download` does."""
    status.put(tag, time.time())
    import huggingface_hub

    model = proof.MODEL or {}  # a shapes-only plan loads no model
    return {"plan": proof.PLAN_ID, "model": model.get("id"), "revision": model.get("revision"), "huggingface_hub": huggingface_hub.__version__,
            "reproduction": {key: os.environ.get(key, "") for key in REPRODUCTION}}


@app.function(image=image, volumes=VOLUMES, cpu=1, memory=4096, timeout=CPU_CALLS["smoke"][1], **CPU_OPTIONS)
def smoke_engine(tag: str) -> dict:
    """The engine image's start without a GPU: this file's import, then the engine's own Python
    importing vLLM and its fused-MoE module, and the tuner's command line."""
    status.put(tag, time.time())
    engine = subprocess.run([proof.engine_python(), "-c", ENGINE_SMOKE], capture_output=True, text=True, env=proof.engine_env({}), timeout=150)
    tuner = subprocess.run([proof.engine_python(), str(Path(tune_moe.__file__)), "--help"], capture_output=True, text=True, env=proof.engine_env({}), timeout=60)
    override = subprocess.run([proof.engine_python(), "-c", OVERRIDE_SMOKE, str(Path(tune_moe.__file__).parent)], capture_output=True, text=True,
                              env=proof.engine_env({}), timeout=150)
    return {"plan": proof.PLAN_ID, "model": (proof.MODEL or {}).get("id"), "reproduction": {key: os.environ.get(key, "") for key in REPRODUCTION},
            "override": json.loads(override.stdout.strip().splitlines()[-1]) if override.returncode == 0 else {"error": (override.stdout + override.stderr)[-800:]},
            "engine": json.loads(engine.stdout.strip().splitlines()[-1]) if engine.returncode == 0 else {"error": (engine.stdout + engine.stderr)[-1500:]},
            "tuner_starts": tuner.returncode == 0, "tuner_error": "" if tuner.returncode == 0 else (tuner.stdout + tuner.stderr)[-800:]}


@app.function(image=download_image, volumes={"/weights": weights}, cpu=2, memory=8192, timeout=CPU_CALLS["download"][1], **CPU_OPTIONS)
def download(tag: str) -> dict:
    """Stages the pinned weights on the volume (CPU only, no GPU time)."""
    status.put(tag, time.time())
    from huggingface_hub import snapshot_download

    marker = Path(proof.MODEL["dir"]) / ".complete"
    if marker.exists() and marker.read_text().strip() == proof.MODEL["revision"]:
        return {"downloaded": False, "path": proof.MODEL["dir"]}
    started = time.monotonic()
    snapshot_download(repo_id=proof.MODEL["id"], revision=proof.MODEL["revision"], local_dir=proof.MODEL["dir"],
                      allow_patterns=["*.json", "*.safetensors", "*.txt", "*.model", "*.jinja"])
    marker.write_text(proof.MODEL["revision"])
    weights.commit()
    files = [path for path in Path(proof.MODEL["dir"]).rglob("*") if path.is_file() and ".cache" not in path.parts]
    return {"downloaded": True, "seconds": round(time.monotonic() - started), "files": len(files), "bytes": sum(path.stat().st_size for path in files)}


PREFLIGHT = r'''
import importlib.metadata, inspect, json, os
import vllm.envs as envs
from vllm.model_executor.layers.fused_moe import fused_moe
folder = os.path.join(os.path.dirname(os.path.realpath(fused_moe.__file__)), "configs")
names = sorted(os.listdir(folder))
import sys
target = sys.argv[1]
print(json.dumps({"vllm": importlib.metadata.version("vllm"), "triton": importlib.metadata.version("triton"), "torch": importlib.metadata.version("torch"),
  "tuned_folder_variable": "VLLM_TUNED_CONFIG_FOLDER" in envs.environment_variables,
  "lookup_reads_folder": "VLLM_TUNED_CONFIG_FOLDER" in inspect.getsource(fused_moe.get_moe_configs),
  "target_file": target, "shipped_target_file": target in names,
  "shipped_same_shape": [n for n in names if n.startswith(target.split(",device_name=")[0] + ",")]}))
'''


@app.function(image=image, volumes=VOLUMES, cpu=2, memory=8192, timeout=CPU_CALLS["preflight"][1], **CPU_OPTIONS)
def preflight(tag: str) -> dict:
    """The image's facts the rules depend on, read with the engine's own Python (no GPU)."""
    status.put(tag, time.time())
    facts: dict = {}
    config = json.loads((Path(proof.MODEL["dir"]) / "config.json").read_text()) if (Path(proof.MODEL["dir"]) / "config.json").exists() else proof.model_config(True)
    target = tune_moe.file_name(tune_moe.shape_from_config(config), PLAN["device"])
    done = subprocess.run([proof.engine_python(), "-c", PREFLIGHT, target], capture_output=True, text=True, env=proof.engine_env({}), timeout=240)
    facts["engine"] = json.loads(done.stdout.strip().splitlines()[-1]) if done.returncode == 0 else {"error": (done.stdout + done.stderr)[-1500:]}
    for key, (path, sha256) in proof.PROMPT_FILES.items():
        import hashlib
        data = Path(path).read_bytes() if Path(path).exists() else b""
        facts[key] = {"exists": bool(data), "sha256_matches": hashlib.sha256(data).hexdigest() == sha256}
    marker = Path(proof.MODEL["dir"]) / ".complete"
    facts["weights"] = marker.read_text().strip() == proof.MODEL["revision"] if marker.exists() else False
    if facts["weights"]:
        facts["shape"] = tune_moe.shape_from_config(json.loads((Path(proof.MODEL["dir"]) / "config.json").read_text())).__dict__
    return facts


GPU_OPTIONS = dict(image=image, gpu=GPU_REQUEST, cpu=GPU_CPU, memory=GPU_MEMORY_MIB, volumes=VOLUMES, **CPU_OPTIONS)


@app.function(timeout=LIMITS.get("shapes", 60), **GPU_OPTIONS)
def shapes_machine(run_id: str, tag: str) -> dict:
    status.put(tag, time.time())
    try:
        return proof.shapes_machine(False, f"/results/{run_id}/shapes", LIMITS["shapes"], log=lambda line: print(line, flush=True))
    finally:
        results.commit()


@app.function(timeout=LIMITS.get("first", 60), **GPU_OPTIONS)
def first_machine(run_id: str, tag: str, kernel_set: dict | None = None) -> dict:
    status.put(tag, time.time())
    try:
        return proof.machine("first", kernel_set, False, f"/results/{run_id}/first", LIMITS["first"], log=lambda line: print(line, flush=True))
    finally:
        results.commit()


@app.function(timeout=LIMITS.get("fresh", 60), **GPU_OPTIONS)
def fresh_machine(run_id: str, tag: str, kernel_set: dict) -> dict:
    status.put(tag, time.time())
    try:
        return proof.machine("fresh", kernel_set, False, f"/results/{run_id}/fresh", LIMITS["fresh"], log=lambda line: print(line, flush=True))
    finally:
        results.commit()


def startup_crashed(call) -> bool:
    """Whether Modal reports the input's container failing at startup. Modal restarts such a
    container without end, so the caller stops at the first sign; a client that cannot read
    the call graph falls back on the start limit."""
    try:
        return any(info.status.name == "INIT_FAILURE" for info in call.get_call_graph())
    except Exception:  # noqa: BLE001 - the start limit still applies
        return False


def wait(call, tag: str, limit: int, start_within: int = QUEUE_SECONDS, what: str = "GPU") -> dict:
    """Waits for one remote call and cancels it at its own wall-clock limits: when it has not
    started within `start_within` seconds (no machine, or a container crashing at startup),
    when it crashed at startup, or `limit` seconds after it started."""
    spawned, started, polls = time.time(), None, 0
    try:
        while True:
            try:
                return call.get(timeout=5)
            except Exception as failure:  # noqa: BLE001 - TimeoutError means still running
                if type(failure).__name__ != "TimeoutError":
                    return {"error": f"{type(failure).__name__}: {str(failure)[-800:]}"}
            now, polls = time.time(), polls + 1
            if started is None:
                started = status.get(tag, None)
                if started:
                    print(f"{tag}: container started after {started - spawned:.0f} s", flush=True)
            if started is None and polls % 3 == 0 and startup_crashed(call):
                return {"error": f"the container crashed at startup after {now - spawned:.0f} s; cancelled", "startup_crash": True}
            if started is None and now - spawned > start_within:
                return {"error": f"the container did not start within {start_within // 60} min (no {what} scheduled, or a crash at startup); cancelled",
                        "not_scheduled": True}
            if started is not None and now - started > limit + CANCEL_MARGIN_SECONDS:
                return {"error": f"cancelled at the wall-clock cap of {limit + CANCEL_MARGIN_SECONDS} s after its start"}
    finally:
        call.cancel(terminate_containers=True)
        try:
            status.pop(tag)
        except Exception:  # noqa: BLE001 - a missing key is fine
            pass


def smoke(run_id: str) -> dict:
    """Both images started in Modal on CPU, each importing this file as every function does, so
    a startup error costs cents instead of a GPU's queue. Run it before any paid call."""
    start_within, limit = CPU_CALLS["smoke"]
    tags = {name: f"{run_id}:smoke-{name}" for name in ("download", "engine")}
    calls = {"download": smoke_download.spawn(tags["download"]), "engine": smoke_engine.spawn(tags["engine"])}
    report = {name: wait(calls[name], tags[name], limit, start_within, "CPU") for name in ("download", "engine")}
    engine = report["engine"].get("engine") or {}
    checks = {
        "download_container_ran": not report["download"].get("error"),
        "engine_container_ran": not report["engine"].get("error"),
        "same_plan_in_both": report["download"].get("plan") == report["engine"].get("plan") == proof.PLAN_ID,
        "reproduction_carried": all(report[name].get("reproduction") == REPRODUCTION for name in ("download", "engine")),
        "engine_imports_vllm": bool(engine.get("vllm")) and not engine.get("error"),
        "tuner_starts": bool(report["engine"].get("tuner_starts")),
        "tuner_restores_the_engine_override": bool((report["engine"].get("override") or {}).get("setting_restores")),
    }
    return {"plan": proof.PLAN_ID, "checks": checks, "passed": all(checks.values()), **report}


def judge(out: Path, fresh: bool) -> dict:
    """The campaign judge's verdict, from the Go command."""
    # The evidence file each verdict's results name: the final verdict's beside it, the
    # first-window verdict's in its own folder, so neither overwrites the other.
    evidence = out if fresh else out / "evidence-first-window"
    evidence.mkdir(parents=True, exist_ok=True)
    command = ["go", "run", "./cmd/infercrane", "optimize", "evidence", "kernel-tuning", "--file", str(out / "window-first.json"),
               "--set", str(out / "kernel-configs.json"), "--evidence-dir", str(evidence), "--output", "json"]
    if fresh:
        command += ["--fresh", str(out / "window-fresh.json")]
    done = subprocess.run(command, cwd=ROOT, capture_output=True, text=True, timeout=900)
    if done.returncode != 0:
        return {"error": (done.stdout + done.stderr)[-2000:]}
    verdict = json.loads(done.stdout)
    (out / ("verdict.json" if fresh else "verdict-first-window.json")).write_text(json.dumps(verdict, indent=1) + "\n")
    return verdict


def first_window_gained(verdict: dict) -> bool:
    if verdict.get("error") or verdict.get("decision") == "reject":
        return False
    return any((cell.get("first", {}).get("comparison", {}).get("output_token_throughput", {}) or {}).get("decision") == "improved" for cell in verdict.get("cells", []))


def save_machine(out: Path, window: str, result: dict) -> None:
    out.mkdir(parents=True, exist_ok=True)
    (out / f"machine-{window}.json").write_text(json.dumps(result, indent=1) + "\n")
    if result.get("record"):
        (out / f"window-{window}.json").write_text(json.dumps(result["record"], indent=1) + "\n")
        (out / f"window-{window}-detail.json").write_text(json.dumps(result["detail"], indent=1) + "\n")
    if result.get("kernel_set"):
        (out / "kernel-configs.json").write_text(json.dumps(result["kernel_set"], indent=1) + "\n")
        (out / "tuning-report.json").write_text(json.dumps(result.get("tuning_report"), indent=1) + "\n")


def load_set(path: str | None) -> dict | None:
    """A tuned set given on the command line, checked against its own digest."""
    if not path:
        return None
    kernel_set = json.loads(Path(path).read_text())
    if "shapes" in kernel_set:
        # A rung 2 result: the set for this plan's model's shape.
        prefix = tune_moe.file_name(tune_moe.shape_from_config(proof.model_config(True)), PLAN["device"])
        matches = [row["set"] for row in kernel_set["shapes"] if row.get("set") and [f["name"] for f in row["set"]["files"]] == [prefix]]
        if len(matches) != 1:
            raise SystemExit(f"{path} holds no single tuned set for {prefix}")
        kernel_set = matches[0]
    if tune_moe.set_digest(kernel_set) != kernel_set.get("digest") or kernel_set.get("device") != PLAN["device"]:
        raise SystemExit(f"{path} does not match its digest or is not for {PLAN['device']}")
    return kernel_set


def dry(out: Path, given: dict | None = None) -> dict:
    """The whole path on this laptop: stand-in tuner timings and a stand-in server. $0."""
    first = proof.machine("first", given, True, str(out / "first"), 900)
    save_machine(out, "first", first)
    if first.get("error") or first.get("stopped"):
        return {"first": first}
    verdict = judge(out, fresh=False)
    fresh = proof.machine("fresh", first["kernel_set"], True, str(out / "fresh"), 900)
    save_machine(out, "fresh", fresh)
    return {"first_window_verdict": verdict, "launch_fresh": first_window_gained(verdict), "verdict": judge(out, fresh=True)}


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__.split("\n\n")[0])
    parser.add_argument("action", choices=("dry", "smoke", "download", "preflight", "run", "shapes"))
    parser.add_argument("--budget-usd", type=float, default=0.0)
    parser.add_argument("--run-id", default=time.strftime("qwen36-moe-h100-%Y%m%dT%H%M%SZ", time.gmtime()))
    parser.add_argument("--out", default=str(ROOT / ".infercrane" / "kernel-tune"))
    parser.add_argument("--set", help="run/dry: measure this tuned set (kernel-configs.json, e.g. rung 2's) instead of tuning on the first machine")
    args = parser.parse_args()
    out = Path(args.out) / args.run_id
    print(f"plan {proof.PLAN_ID} (rules tools/kernel-tune/{PLAN['rules']})", flush=True)
    if SHAPES_ONLY and args.action not in ("dry", "smoke", "shapes"):
        raise SystemExit(f"plan {proof.PLAN_ID} tunes shapes only: use the dry, smoke or shapes action")
    if not SHAPES_ONLY and args.action == "shapes":
        raise SystemExit(f"plan {proof.PLAN_ID} is an end-to-end proof: use download, preflight and run")
    if args.action == "dry" and SHAPES_ONLY:
        result = proof.shapes_machine(True, str(out / "dry"), LIMITS["shapes"])
        print(json.dumps({"error": result.get("error"), "shipped": result.get("shipped"), "verdict": result.get("verdict")}, indent=1))
        print(f"wrote {out / 'dry'}")
        return
    if args.action == "dry":
        print(json.dumps(dry(out / "dry", load_set(args.set)), indent=1)[:6000])
        print(f"wrote {out / 'dry'}")
        return
    with modal.enable_output(), app.run():
        if args.action == "smoke":
            report = smoke(args.run_id)
            print(json.dumps(report, indent=1), flush=True)
            if not report["passed"]:
                raise SystemExit("smoke check failed: launch nothing on a GPU from this commit")
            return
        if args.action in ("download", "preflight"):
            start_within, limit = CPU_CALLS[args.action]
            tag = f"{args.run_id}:{args.action}"
            print(f"{args.action}: at most {start_within // 60} min to start and {limit // 60} min after", flush=True)
            result = wait((download if args.action == "download" else preflight).spawn(tag), tag, limit, start_within, "CPU")
            print(json.dumps(result, indent=1), flush=True)
            if result.get("error"):
                raise SystemExit(f"{args.action} failed")
            return
        cap = cap_usd()
        if args.action == "shapes":
            if args.budget_usd < cap:
                raise SystemExit(f"--budget-usd {args.budget_usd} is below the machine's cap at list prices, ${cap}")
            print(f"cap: {LIMITS['shapes'] // 60} min after it starts; at most ${cap} at ${rate_per_second() * 3600:.2f}/h", flush=True)
            tag = f"{args.run_id}:shapes"
            result = wait(shapes_machine.spawn(args.run_id, tag), tag, LIMITS["shapes"])
            out.mkdir(parents=True, exist_ok=True)
            (out / "shapes-result.json").write_text(json.dumps(result, indent=1) + "\n")
            print(json.dumps({"error": result.get("error"), "shipped": result.get("shipped"), "verdict": result.get("verdict"),
                              "seconds": result.get("container_seconds")}, indent=1), flush=True)
            print(f"wrote {out}; bill: modal billing report --for today --show-resources", flush=True)
            return
        if args.budget_usd < cap:
            raise SystemExit(f"--budget-usd {args.budget_usd} is below the two machines' caps at list prices, ${cap}")
        print(f"caps: first {LIMITS['first'] // 60} min, fresh {LIMITS['fresh'] // 60} min after each starts; at most ${cap} at ${rate_per_second() * 3600:.2f}/h", flush=True)
        tag = f"{args.run_id}:first"
        given = load_set(args.set)
        first = wait(first_machine.spawn(args.run_id, tag, given), tag, LIMITS["first"])
        save_machine(out, "first", first)
        print(f"first machine: {first.get('error') or first.get('stopped') or 'window measured'}; {first.get('container_seconds', '?')} s", flush=True)
        if not first.get("record") or first.get("error") or first.get("stopped"):
            return
        verdict = judge(out, fresh=False)
        print(json.dumps(verdict, indent=1)[:4000], flush=True)
        if not first_window_gained(verdict):
            print("the first window showed no gain the judge calls improved (or a rejection): no fresh machine", flush=True)
            return
        tag = f"{args.run_id}:fresh"
        fresh = wait(fresh_machine.spawn(args.run_id, tag, first["kernel_set"]), tag, LIMITS["fresh"])
        save_machine(out, "fresh", fresh)
        print(f"fresh machine: {fresh.get('error') or fresh.get('stopped') or 'window measured'}; {fresh.get('container_seconds', '?')} s", flush=True)
        if fresh.get("record"):
            print(json.dumps(judge(out, fresh=True), indent=1)[:6000], flush=True)
        print(f"wrote {out}; bill: modal billing report --for today --show-resources", flush=True)


if __name__ == "__main__":
    main()
