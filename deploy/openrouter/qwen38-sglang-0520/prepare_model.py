#!/usr/bin/env python3
"""Populate the immutable Qwen3.8 artifact on a persistent volume."""

from __future__ import annotations

import hashlib
import json
import pathlib

from huggingface_hub import snapshot_download

MODEL = "Qwen/Qwen3.8-27B-FP8"
REVISION = "017b9c7af6b5689d5dd426a76e0bc077eb5ca20a"
ROOT = pathlib.Path("/runpod-volume/infercrane/qwen38")

ROOT.mkdir(parents=True, exist_ok=True)
model_path = ROOT / "model"
snapshot_download(
    repo_id=MODEL,
    revision=REVISION,
    local_dir=model_path,
    local_dir_use_symlinks=False,
)

files = []
for path in sorted(item for item in model_path.rglob("*") if item.is_file()):
    digest = hashlib.sha256()
    with path.open("rb") as handle:
        for chunk in iter(lambda: handle.read(8 << 20), b""):
            digest.update(chunk)
    files.append(
        {
            "path": str(path.relative_to(model_path)),
            "bytes": path.stat().st_size,
            "sha256": digest.hexdigest(),
        }
    )

manifest = {
    "schema_version": "infercrane.dev/model-artifact/v1",
    "model": MODEL,
    "revision": REVISION,
    "files": files,
}
(ROOT / "manifest.json").write_text(
    json.dumps(manifest, indent=2, sort_keys=True) + "\n", encoding="utf-8"
)
print(ROOT / "manifest.json")
