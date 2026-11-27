"""Builds the state-by-draft engine image from this folder's Dockerfile in Modal's image builder and
checks it there on a CPU container: the receipt verifies (verify_state_by_draft.py), vLLM imports,
the scheduler module carries the patch, and the engine's own receipt check (v4) passes. Nothing is pushed to any registry. Cost: a CPU
container for a few minutes; the build itself runs on Modal's builder.

    uv run --no-project --with modal==1.6.4 modal run deploy/vllm-state-by-draft/modal_image_check.py
"""

from __future__ import annotations

import json
import pathlib
import subprocess

import modal

HERE = pathlib.Path(__file__).resolve().parent
# Inside the container this file is /root/modal_image_check.py, with no repository around it;
# the image is built from the laptop's checkout only.
ROOT = HERE.parents[1] if len(HERE.parents) > 1 else HERE

app = modal.App("infercrane-state-by-draft-image-check")
# Modal runs the function on its own Python; the engine keeps the image's Python.
image = modal.Image.from_dockerfile(str(HERE / "Dockerfile"), context_dir=str(ROOT), add_python="3.12").entrypoint([])


def engine_python() -> str:
    for python in ("/opt/venv/bin/python", "/usr/bin/python3", "/usr/local/bin/python3"):
        if pathlib.Path(python).exists() and subprocess.run([python, "-c", "import vllm"], capture_output=True, env=clean_env()).returncode == 0:
            return python
    raise RuntimeError("the image has no Python that can import vllm")


def clean_env() -> dict:
    import os

    env = dict(os.environ)
    env.pop("PYTHONHOME", None)
    env["PYTHONPATH"] = ":".join(part for part in env.get("PYTHONPATH", "").split(":") if part and not part.startswith(("/pkg", "/root", "/__modal")))
    return env


@app.function(image=image, cpu=2.0, memory=8192, timeout=15 * 60, retries=0, max_containers=1, single_use_containers=True)
def check() -> dict:
    python = engine_python()
    out: dict = {"python": python}
    for name, command in (("verify", [python, "/opt/infercrane/verify_state_by_draft.py", "/opt/infercrane/state-by-draft-receipt.json"]),
                          ("vllm", [python, "-c", "import vllm; print(vllm.__version__)"]),
                          ("check", [python, "/opt/infercrane/check_state_by_draft.py"])):
        done = subprocess.run(command, capture_output=True, text=True, timeout=600, env=clean_env())
        out[name] = {"returncode": done.returncode, "stdout": done.stdout[-4000:], "stderr": done.stderr[-1500:] if done.returncode else ""}
    out["receipt"] = json.loads(pathlib.Path("/opt/infercrane/state-by-draft-receipt.json").read_text())
    # The entrypoint: the receipt check passes and it hands its arguments to `vllm serve`. A stand-in
    # `vllm` first on PATH records the hand-over; vLLM's own CLI needs a GPU to build its parser.
    stub = pathlib.Path("/tmp/stub-bin")
    stub.mkdir(exist_ok=True)
    (stub / "vllm").write_text('#!/bin/sh\necho "handed to: vllm $*"\n')
    (stub / "vllm").chmod(0o755)
    env = clean_env()
    env["PATH"] = f"{stub}:{env.get('PATH', '')}"
    done = subprocess.run(["/opt/infercrane/serve-state-by-draft", "/weights/model", "--port", "8000"], capture_output=True, text=True, timeout=600, env=env)
    out["wrapper"] = {"returncode": done.returncode, "reached_vllm_serve": "handed to: vllm serve /weights/model --port 8000" in done.stdout,
                      "stdout": done.stdout[-300:], "stderr": done.stderr[-1500:]}
    # The setup needs vLLM's Python front end: the wrapper refuses a Rust one before anything else.
    done = subprocess.run(["/opt/infercrane/serve-state-by-draft", "--help"], capture_output=True, text=True, timeout=120,
                          env=dict(clean_env(), VLLM_USE_RUST_FRONTEND="1"))
    out["rust_front_end"] = {"returncode": done.returncode, "stderr": done.stderr[-600:]}
    # Fail closed: a receipt that names another patch is refused.
    tampered = dict(out["receipt"], patch="infercrane-state-by-draft-v0")
    pathlib.Path("/tmp/tampered-receipt.json").write_text(json.dumps(tampered))
    done = subprocess.run([python, "/opt/infercrane/verify_state_by_draft.py", "/tmp/tampered-receipt.json"], capture_output=True, text=True, timeout=300, env=clean_env())
    out["tampered_receipt"] = {"returncode": done.returncode, "stdout": done.stdout[-1000:]}
    return out


stock_image = modal.Image.from_registry("vllm/vllm-openai@sha256:cf5b2fdd087eb4f6d261736967cc5cde974bd22ff4df50365f8fa419b6b4c484", add_python="3.12").entrypoint([])


@app.function(image=stock_image, cpu=1.0, memory=2048, timeout=10 * 60, retries=0, max_containers=1, single_use_containers=True)
def stock() -> dict:
    """On stock vLLM the technique's command does not exist, so its setup cannot start there."""
    return {"wrapper_present": pathlib.Path("/opt/infercrane/serve-state-by-draft").exists(),
            "receipt_present": pathlib.Path("/opt/infercrane/state-by-draft-receipt.json").exists()}


@app.local_entrypoint()
def main() -> None:
    result = check.remote()
    plain = stock.remote()
    print(json.dumps({key: value for key, value in result.items() if key != "receipt"}, indent=1))
    print("receipt:", json.dumps(result["receipt"]))
    print("stock vLLM image:", json.dumps(plain))
    ok = (all(result[name]["returncode"] == 0 for name in ("verify", "vllm", "check")) and result["wrapper"]["returncode"] == 0
          and result["wrapper"]["reached_vllm_serve"] and result["tampered_receipt"]["returncode"] != 0 and result["rust_front_end"]["returncode"] == 78 and not plain["wrapper_present"])
    print("image check:", "passed" if ok else "FAILED")
    if not ok:
        raise SystemExit(1)
