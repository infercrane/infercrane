#!/usr/bin/env python3
"""A CPU check of the state-by-draft patch, run with the engine's own Python after the patch.

It builds vLLM's own MambaManager (mamba cache mode "none", 3 speculative blocks) over a block
pool and admits requests reserving 3, 1 and 0 draft tokens' state, and one with no reservation,
through the same calls the scheduler makes; then checks the scheduler module carries the patch.
No GPU: the kernels are checked on one by the gate.
"""

from __future__ import annotations

import inspect
import json
import pathlib
import sys


def main() -> int:
    import torch
    from vllm.v1.core import single_type_kv_cache_manager as managers
    from vllm.v1.core.block_pool import BlockPool
    from vllm.v1.core.sched import scheduler
    from vllm.v1.kv_cache_interface import MambaSpec

    results = {"checks": []}

    def check(name: str, ok: bool, detail) -> None:
        results["checks"].append({"check": name, "ok": bool(ok), "detail": detail})

    spec = MambaSpec(block_size=4096, shapes=((4, 4),), dtypes=(torch.float32,), mamba_cache_mode="none", num_speculative_blocks=3)
    pool = BlockPool(num_gpu_blocks=64, enable_caching=False, hash_block_size=4096)
    manager = managers.MambaManager(spec, pool, enable_caching=False, kv_cache_group_id=0, scheduler_block_size=4096)
    held = {}
    for request_id, reserved in (("three", 3), ("one", 1), ("none", 0), ("unlisted", None)):
        if reserved is not None:
            managers.SPEC_RESERVATION[request_id] = reserved
        needed = manager.get_num_blocks_to_allocate(request_id, 300, [], 0, 0, 300)
        manager.allocate_new_blocks(request_id, 300, 300)
        # A decode step asks for one more token; the request keeps its blocks.
        manager.allocate_new_blocks(request_id, 301, 301)
        held[request_id] = {"asked": needed, "held": len(manager.req_to_blocks[request_id])}
    check("each request holds 1 + its reserved draft tokens' state blocks", held == {
        "three": {"asked": 4, "held": 4}, "one": {"asked": 2, "held": 2}, "none": {"asked": 1, "held": 1}, "unlisted": {"asked": 4, "held": 4}}, held)
    check("the pool gave out exactly those blocks", pool.get_num_free_blocks() == 64 - 1 - (4 + 2 + 1 + 4), pool.get_num_free_blocks())

    source = inspect.getsource(scheduler.Scheduler)
    check("the scheduler shares the manager's reservation table", scheduler.SPEC_RESERVATION is managers.SPEC_RESERVATION, None)
    check("the scheduler reserves at admission, clamps each step's drafts and drops a finished request's reservation",
          "SPEC_RESERVATION[request.request_id] = self.dynamic_sd_lookup[" in source and "SPEC_RESERVATION.get(req_id, self.num_spec_tokens)" in source
          and "SPEC_RESERVATION.pop(request.request_id, None)" in source, None)
    check("the scheduler counts admissions by reservation and logs the running mix", scheduler.SPEC_ADMITTED is managers.SPEC_ADMITTED
          and "InferCrane state by draft: running by reserved drafts" in source, None)
    # v3: the mix travels in the scheduler's stats to vLLM's Prometheus logger.
    from vllm.v1.metrics import loggers, stats

    check("the scheduler's stats carry the running requests by reserved draft tokens",
          stats.SchedulerStats().running_by_reserved_drafts == {} and "running_by_reserved_drafts=self._infercrane_reservation_mix()" in source
          and hasattr(scheduler.Scheduler, "_infercrane_reservation_mix"), None)
    logger_source = inspect.getsource(loggers.PrometheusStatLogger)
    check("vLLM's Prometheus logger exports the reservation gauges",
          all(name in logger_source for name in ("vllm:infercrane_running_reserving_{drafts}_drafts", "vllm:infercrane_running_mixed_reservations",
                                                  "scheduler_stats.running_by_reserved_drafts", "vllm:infercrane_state_by_draft_receipt_verified")), None)
    # v4: the engine checks its own receipt when the logger starts (run after the receipt is written).
    if pathlib.Path("/opt/infercrane/state-by-draft-receipt.json").exists():
        check("the engine's own receipt check passes", loggers._infercrane_receipt_verified() == 1, None)
    results["passed"] = all(entry["ok"] for entry in results["checks"])
    print(json.dumps(results, indent=1))
    return 0 if results["passed"] else 1


if __name__ == "__main__":
    sys.exit(main())
