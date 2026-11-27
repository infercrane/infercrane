#!/usr/bin/env python3
"""Checks that an engine image carries InferCrane's state-by-draft patch as it was built.

The receipt (written by patch_state_by_draft.py at build) must name the patch and both vLLM files
as patched, and each installed file's sha256 must equal the after-sha256 pinned here. That is the
source the H100 proofs ran, so a receipt alone, or a file edited after the build, does not pass.
Exit 0 when it holds; otherwise non-zero with the reason. Run with the engine's own Python, before
the engine starts with the technique's flags; on any failure the starting setup serves instead.

    python3 verify_state_by_draft.py [RECEIPT]   (default /opt/infercrane/state-by-draft-receipt.json)
"""

from __future__ import annotations

import hashlib
import importlib.util
import json
import pathlib
import sys

PATCH_ID = "infercrane-state-by-draft-v4"
RUNTIME = "vllm-0.34.0"
# The patched sources' sha256, as built from vLLM 0.34.0 by patch_state_by_draft.py (v4: v2's
# reservation edits, which the H100 proofs ran, the reservation gauges, and the engine's own
# receipt check).
AFTER_SHA256 = {
    "v1/core/single_type_kv_cache_manager.py": "c89ac08ce796f114523e4943697f192f3afdb115c689a09e08aee999ef28494b",
    "v1/core/sched/scheduler.py": "f0b66f799d510fe36329b4bb49c9db38feb2d4b8b215ddc2306351e42cecb662",
    "v1/metrics/stats.py": "55eb30e32426f1d3016ef6a3f07a9d4e40740e9e39494ba0b73a33f8dd310773",
    "v1/metrics/loggers.py": "dca681c5eeed5ea0ab911ed27b851554c5faf7c10684f9ff07542d47115a6867",
}
DEFAULT_RECEIPT = "/opt/infercrane/state-by-draft-receipt.json"


def verify(receipt: dict, package: pathlib.Path) -> list[str]:
    """Every reason the image does not carry the patch as built; empty when it does."""
    problems = []
    if receipt.get("patch") != PATCH_ID:
        problems.append(f"the receipt names patch {receipt.get('patch')!r}, not {PATCH_ID}")
    if receipt.get("runtime") != RUNTIME:
        problems.append(f"the receipt names runtime {receipt.get('runtime')!r}, not {RUNTIME}")
    entries = {entry.get("file"): entry for entry in receipt.get("files") or []}
    for relative, expected in AFTER_SHA256.items():
        entry = entries.get(relative)
        if not entry or entry.get("state") != "patched":
            problems.append(f"the receipt does not show {relative} patched")
            continue
        if entry.get("after_sha256") != expected:
            problems.append(f"the receipt's sha256 for {relative} is not the built one")
        path = package / relative
        try:
            found = hashlib.sha256(path.read_bytes()).hexdigest()
        except OSError as failure:
            problems.append(f"{relative} cannot be read: {failure}")
            continue
        if found != expected:
            problems.append(f"{relative} has sha256 {found}, not the built {expected}")
    return problems


def main(argv: list[str]) -> int:
    receipt_path = pathlib.Path(argv[0] if argv else DEFAULT_RECEIPT)
    try:
        receipt = json.loads(receipt_path.read_text())
    except (OSError, ValueError) as failure:
        print(json.dumps({"verified": False, "problems": [f"no readable receipt at {receipt_path}: {failure}"]}))
        return 2
    spec = importlib.util.find_spec("vllm")
    if spec is None or not spec.submodule_search_locations:
        print(json.dumps({"verified": False, "problems": ["the installed vLLM package is not found"]}))
        return 2
    problems = verify(receipt, pathlib.Path(next(iter(spec.submodule_search_locations))))
    print(json.dumps({"verified": not problems, "patch": receipt.get("patch"), "problems": problems}))
    return 0 if not problems else 1


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
