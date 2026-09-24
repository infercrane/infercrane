"""Independent external monitor for the InferCrane OpenRouter provider path.

This app deliberately owns no GPU. Keeping the release monitor separate from
the immutable model-serving app lets blue/green releases retire old H200s
without interrupting the public soak or coupling observability to capacity.
"""

from __future__ import annotations

import json
import os
import re
import time
import urllib.request
from pathlib import Path

import modal


APP_NAME = "infercrane-openrouter-monitor"
PROVIDER_INGRESS = os.environ.get(
    "INFERCRANE_PROVIDER_INGRESS", "https://provider.infercrane.com"
).rstrip("/")
PUBLIC_MODEL_ID = "qwen/qwen3.8-27b"

app = modal.App(APP_NAME)
evidence = modal.Volume.from_name(
    "infercrane-qwen38-optimization-results", create_if_missing=False
)
probe_secret = modal.Secret.from_name("infercrane-openrouter-provider")


@app.function(
    image=modal.Image.debian_slim(python_version="3.12"),
    volumes={"/vol/evidence": evidence},
    secrets=[probe_secret],
    schedule=modal.Period(minutes=1),
    timeout=90,
)
def external_provider_probe() -> None:
    """Verify the public streaming path and persist content-free evidence."""

    provider_key = os.environ.get("INFERCRANE_OPENROUTER_API_KEY", "").strip()
    if not provider_key:
        raise RuntimeError("provider key is required for the external probe")

    started_wall = time.time()
    started = time.monotonic()
    payload = json.dumps(
        {
            "model": PUBLIC_MODEL_ID,
            "messages": [{"role": "user", "content": "Reply with ready."}],
            "temperature": 0,
            "max_tokens": 16,
            "stream": True,
            "stream_options": {"include_usage": True},
        }
    ).encode()
    request = urllib.request.Request(
        PROVIDER_INGRESS + "/v1/chat/completions",
        data=payload,
        method="POST",
        headers={
            "Authorization": "Bearer " + provider_key,
            "Content-Type": "application/json",
        },
    )
    receipt: dict[str, object] = {
        "schema_version": "infercrane.dev/openrouter-provider-soak/v1",
        "started_at_unix_ms": round(started_wall * 1000),
        "endpoint": PROVIDER_INGRESS,
        "model": PUBLIC_MODEL_ID,
        "status": 0,
        "duration_ms": 0,
        "ttft_ms": 0,
        "prompt_tokens": 0,
        "completion_tokens": 0,
        "saw_done": False,
        "saw_finish_reason": False,
        "passed": False,
    }
    try:
        with urllib.request.urlopen(request, timeout=75) as response:
            receipt["status"] = response.status
            first_event = 0.0
            for raw_line in response:
                line = raw_line.decode("utf-8", errors="replace").strip()
                if not line.startswith("data:"):
                    continue
                data = line.removeprefix("data:").strip()
                if data == "[DONE]":
                    receipt["saw_done"] = True
                    continue
                event = json.loads(data)
                choices = event.get("choices") or []
                if choices and first_event == 0:
                    first_event = time.monotonic()
                if any(choice.get("finish_reason") for choice in choices):
                    receipt["saw_finish_reason"] = True
                usage = event.get("usage") or {}
                receipt["prompt_tokens"] = usage.get(
                    "prompt_tokens", receipt["prompt_tokens"]
                )
                receipt["completion_tokens"] = usage.get(
                    "completion_tokens", receipt["completion_tokens"]
                )
            if first_event:
                receipt["ttft_ms"] = round((first_event - started) * 1000, 3)
            receipt["passed"] = bool(
                response.status == 200
                and receipt["saw_done"]
                and receipt["saw_finish_reason"]
                and receipt["prompt_tokens"]
                and receipt["completion_tokens"]
            )
    except Exception as exc:
        receipt["error_type"] = type(exc).__name__
        receipt["error"] = str(exc)[:512]
    finally:
        receipt["duration_ms"] = round((time.monotonic() - started) * 1000, 3)
        probe_dir = Path("/vol/evidence/openrouter-provider-soak")
        probe_dir.mkdir(parents=True, exist_ok=True)
        task_id = re.sub(
            r"[^A-Za-z0-9_.-]", "_", os.environ.get("MODAL_TASK_ID", "probe")
        )
        destination = probe_dir / f"probe-{int(started_wall * 1000)}-{task_id}.json"
        temporary = destination.with_suffix(".tmp")
        temporary.write_text(json.dumps(receipt, sort_keys=True) + "\n")
        temporary.chmod(0o600)
        temporary.replace(destination)
        evidence.commit()

	if not receipt["passed"]:
		raise RuntimeError("external provider probe failed; receipt persisted")
