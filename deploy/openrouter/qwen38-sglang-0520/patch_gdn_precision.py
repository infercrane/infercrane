#!/usr/bin/env python3
"""Apply the exact reviewed GDN FP32-state fixes missing from SGLang 0.5.20."""

from __future__ import annotations

import hashlib
import importlib.util
import json
import pathlib

PATCHES = {
    "kernels/ops/attention/fla/fused_recurrent.py": {
        "before_sha256": "35a928d24bf6cc3ca56d73e4b729ec004ec8a34760e2425e2f04d6ef783db9f8",
        "after_sha256": "5fcf3e425d41c5544948b512e1b534897f04bdbac9ac1619ffe224cbb960477a",
        "old": "beta_val = tl.sigmoid(b_val).to(b.dtype.element_ty).to(tl.float32)",
        "new": "beta_val = tl.sigmoid(b_val)",
        "upstream": "sgl-project/sglang#38977@301780e843493a9cabcab32a5de727c7715143c2",
    },
    "kernels/ops/attention/fla/fused_recurrent_linear_replayssm.py": {
        "before_sha256": "a8824a71ab49fde1f070c325c89603e6198928bdcb2238f2d6e9ef8fb7247f62",
        "after_sha256": "a18e01ba74904e8f7d27f1eb2d886af5c139b3de8f2a84290c71b5c041a5aa1a",
        "old": "beta_val = tl.sigmoid(b_val).to(b.dtype.element_ty).to(tl.float32)",
        "new": "beta_val = tl.sigmoid(b_val)",
        "upstream": "sgl-project/sglang#38977@301780e843493a9cabcab32a5de727c7715143c2",
    },
    "kernels/ops/attention/fla/fused_gdn_gating.py": {
        "before_sha256": "c7736d1e506fb2c3e5c0496a2ed8c23347c2507ebe73c08bc6745517495d0957",
        "after_sha256": "7318818b58efcb17803b52f236319eda5cc47b813d64113d1c7b83dd2cac378d",
        "old": "tl.store(beta_output + off, blk_beta_output.to(b.dtype.element_ty), mask=mask)",
        "new": "tl.store(beta_output + off, blk_beta_output, mask=mask)",
        "upstream": "sgl-project/sglang#40362@1f313ba3263f2043c1adf4ccdd36483010d43551",
    },
}


def digest(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


def patch_text(text: str, old: str, new: str) -> str:
    if text.count(old) != 1:
        raise ValueError("expected vulnerable GDN expression exactly once")
    updated = text.replace(old, new)
    if old in updated or updated.count(new) < 1:
        raise ValueError("GDN precision patch did not produce the expected source")
    return updated


def main() -> None:
    spec = importlib.util.find_spec("sglang")
    if spec is None or not spec.submodule_search_locations:
        raise SystemExit("installed SGLang package is unavailable")
    package = pathlib.Path(next(iter(spec.submodule_search_locations)))
    receipt = {
        "schema_version": "infercrane.dev/runtime-source-patch/v1",
        "runtime": "sglang-0.5.20",
        "patches": [],
    }
    for relative, patch in PATCHES.items():
        path = package / relative
        before = path.read_bytes()
        current_digest = digest(before)
        already_applied = current_digest == patch["after_sha256"]
        if current_digest not in {patch["before_sha256"], patch["after_sha256"]}:
            raise SystemExit(f"refusing to patch unexpected SGLang source: {relative}")
        if already_applied:
            text = before.decode()
            if patch["old"] in text or text.count(patch["new"]) < 1:
                raise SystemExit(f"patched SGLang source failed semantic validation: {relative}")
        else:
            path.write_text(patch_text(before.decode(), patch["old"], patch["new"]))
        after_digest = digest(path.read_bytes())
        if after_digest != patch["after_sha256"]:
            raise SystemExit(f"patched SGLang source digest mismatch: {relative}")
        receipt["patches"].append(
            {
                "path": relative,
                "before_sha256": patch["before_sha256"],
                "after_sha256": after_digest,
                "already_applied": already_applied,
                "upstream": patch["upstream"],
            }
        )
    destination = pathlib.Path("/opt/infercrane/gdn-precision-patch.json")
    destination.parent.mkdir(parents=True, exist_ok=True)
    destination.write_text(json.dumps(receipt, indent=2, sort_keys=True) + "\n")
    print(destination)


if __name__ == "__main__":
    main()
