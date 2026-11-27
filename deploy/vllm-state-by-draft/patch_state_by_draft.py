#!/usr/bin/env python3
"""InferCrane's state-by-draft-length patch for vLLM 0.34.0 (prototype, prefix caching off).

vLLM's dynamic speculative decoding (`num_speculative_tokens_per_batch_size`) lowers the draft
length as the batch grows, but every request of a model with recurrent-state layers still reserves
one copy of the state per draft token of the longest length (`MambaSpec.num_speculative_blocks`).
On Qwen3.8-27B that is 13 cache blocks per short request with 3 drafts against 7 with 1 and 4 with
none, so the engine never runs more than three drafts' room whatever the schedule says.

This patch makes the reservation follow the schedule, in mamba cache mode "none" (prefix caching
off; mode "align" is unchanged and keeps the full reservation):

1. When the scheduler admits a request, it reserves the speculative state of the draft length
   the schedule gives the batch the request joins (running + 1), for the request's whole life.
2. A step drafts no more tokens than the smallest reservation among the requests it schedules,
   so the recurrent kernels, which read a request's state at column accepted-1 and write one
   column per query token, stay inside every request's reserved columns.
3. A finished request's reservation is dropped.
4. Every 50 admissions the scheduler logs the running requests by the drafts they reserved, so a
   run can show that requests reserving different lengths ran together.
5. (v3) The scheduler's stats carry the running requests by reserved draft tokens, and vLLM's
   Prometheus logger exports them on /metrics: vllm:infercrane_running_reserving_{0,1,3}_drafts
   and vllm:infercrane_running_mixed_reservations (the smaller of those reserving three and those
   reserving fewer), so the product records the mix with the engine's other metrics. Edits 1-4 are
   v2's, unchanged; v2 is what the H100 proofs ran.
6. (v4) When vLLM's Prometheus logger starts, the engine checks the image's receipt and the
   patched files' sha256 itself (/opt/infercrane/verify_state_by_draft.py) and exports the answer
   as vllm:infercrane_state_by_draft_receipt_verified, 1 or 0, so the product reads the engine's
   own check on any deploy path, a proxy included.

Run with the engine's own Python before the engine starts. It refuses any source file whose
sha256 it does not know, applies each edit exactly once, checks the result's sha256, and writes
a receipt. With --check it only reports whether the installed files are the known ones. With
--backup DIR it first copies the two original files there; --restore DIR puts them back, checks
their sha256 and drops the bytecode, so engines started afterwards run vLLM as released.

    /opt/venv/bin/python vllm_state_by_draft.py [--receipt PATH] [--check] [--backup DIR]
    /opt/venv/bin/python vllm_state_by_draft.py --restore DIR [--receipt PATH]
"""

from __future__ import annotations

import hashlib
import importlib.util
import json
import pathlib
import shutil
import sys

VERSION = "0.34.0"
PATCH_ID = "infercrane-state-by-draft-v4"

EDITS = {
    "v1/core/single_type_kv_cache_manager.py": {
        "before_sha256": "a1266bb386044c6b8d60930a549115448339e43ecba188e1d431eb1d535cfa53",
        "edits": [
            ("\n\nclass MambaManager(SingleTypeKVCacheManager):\n",
             "\n\n# InferCrane patch (state by draft length): the speculative state blocks each\n"
             "# request reserves, by request id, as the scheduler chose them at admission. A\n"
             "# request not listed reserves num_speculative_blocks, as without the patch.\n"
             "SPEC_RESERVATION: dict[str, int] = {}\n"
             "# Admissions by the draft tokens' state they reserved, for the scheduler's log.\n"
             "SPEC_ADMITTED: dict[int, int] = {}\n"
             "\n\nclass MambaManager(SingleTypeKVCacheManager):\n"),
            ("            if self.num_speculative_blocks > 0:\n"
             "                num_tokens += (\n"
             "                    self.kv_cache_spec.block_size * self.num_speculative_blocks\n"
             "                )\n",
             "            reserved = SPEC_RESERVATION.get(request_id, self.num_speculative_blocks)\n"
             "            if reserved > 0:\n"
             "                num_tokens += self.kv_cache_spec.block_size * reserved\n"),
            ("            if self.num_speculative_blocks > 0:\n"
             "                num_tokens += self.block_size * self.num_speculative_blocks\n"
             "            return super().allocate_new_blocks(\n",
             "            reserved = SPEC_RESERVATION.get(request_id, self.num_speculative_blocks)\n"
             "            if reserved > 0:\n"
             "                num_tokens += self.block_size * reserved\n"
             "            return super().allocate_new_blocks(\n"),
        ],
    },
    "v1/core/sched/scheduler.py": {
        "before_sha256": "8f5bd5d0a4ee344d23466fdb842716042673f6c9a081d5c80bae565a9d514b41",
        "edits": [
            ("from vllm.v1.core.kv_cache_utils import KVCacheBlock\n",
             "from vllm.v1.core.kv_cache_utils import KVCacheBlock\n"
             "from vllm.v1.core.single_type_kv_cache_manager import SPEC_ADMITTED, SPEC_RESERVATION\n"),
            ("                num_tokens_past_hit = max(num_new_tokens - num_replay_tokens, 0)\n"
             "                new_blocks = self.kv_cache_manager.allocate_slots(\n",
             "                num_tokens_past_hit = max(num_new_tokens - num_replay_tokens, 0)\n"
             "                if self.dynamic_sd_lookup is not None:\n"
             "                    # InferCrane patch (state by draft length): a request reserves\n"
             "                    # the state of the draft length the schedule gives the batch\n"
             "                    # it joins, for its whole life.\n"
             "                    SPEC_RESERVATION[request.request_id] = self.dynamic_sd_lookup[\n"
             "                        min(len(self.running) + 1, len(self.dynamic_sd_lookup) - 1)\n"
             "                    ]\n"
             "                new_blocks = self.kv_cache_manager.allocate_slots(\n"),
            ("        if self.dynamic_sd_lookup is not None and len(num_scheduled_tokens) > 0:\n"
             "            num_spec_tokens_to_schedule = self.dynamic_sd_lookup[\n"
             "                len(num_scheduled_tokens)\n"
             "            ]\n",
             "        if self.dynamic_sd_lookup is not None and len(num_scheduled_tokens) > 0:\n"
             "            num_spec_tokens_to_schedule = self.dynamic_sd_lookup[\n"
             "                len(num_scheduled_tokens)\n"
             "            ]\n"
             "            # InferCrane patch (state by draft length): never draft more than\n"
             "            # the request with the least speculative state reserved can hold.\n"
             "            num_spec_tokens_to_schedule = min(\n"
             "                [num_spec_tokens_to_schedule]\n"
             "                + [SPEC_RESERVATION.get(req_id, self.num_spec_tokens) for req_id in num_scheduled_tokens]\n"
             "            )\n"),
            ("                    break\n"
             "\n"
             "                # KVTransfer: the connector uses this info to determine\n",
             "                    break\n"
             "\n"
             "                if self.dynamic_sd_lookup is not None:\n"
             "                    # InferCrane patch (state by draft length): count admissions by\n"
             "                    # the state they reserved and, every 50, log how the running\n"
             "                    # requests' reservations mix.\n"
             "                    held = SPEC_RESERVATION[request.request_id]\n"
             "                    SPEC_ADMITTED[held] = SPEC_ADMITTED.get(held, 0) + 1\n"
             "                    if sum(SPEC_ADMITTED.values()) % 50 == 0:\n"
             "                        mix: dict[int, int] = {}\n"
             "                        for other in self.running:\n"
             "                            key = SPEC_RESERVATION.get(other.request_id, self.num_spec_tokens)\n"
             "                            mix[key] = mix.get(key, 0) + 1\n"
             "                        logger.info(\n"
             "                            \"InferCrane state by draft: running by reserved drafts %s, \"\n"
             "                            \"admitted by reserved drafts %s\",\n"
             "                            dict(sorted(mix.items())),\n"
             "                            dict(sorted(SPEC_ADMITTED.items())),\n"
             "                        )\n"
             "\n"
             "                # KVTransfer: the connector uses this info to determine\n"),
            ("            ec_connector_stats=ec_connector_stats_payload,\n"
             "        )\n",
             "            ec_connector_stats=ec_connector_stats_payload,\n"
             "            running_by_reserved_drafts=self._infercrane_reservation_mix(),\n"
             "        )\n"),
            ("    def make_spec_decoding_stats(\n",
             "    def _infercrane_reservation_mix(self) -> dict[int, int]:\n"
             "        # InferCrane patch (state by draft length): the running requests by\n"
             "        # the draft tokens' state each reserved.\n"
             "        if self.dynamic_sd_lookup is None:\n"
             "            return {}\n"
             "        mix: dict[int, int] = {}\n"
             "        for running_request in self.running:\n"
             "            held = SPEC_RESERVATION.get(running_request.request_id, self.num_spec_tokens)\n"
             "            mix[held] = mix.get(held, 0) + 1\n"
             "        return mix\n"
             "\n"
             "    def make_spec_decoding_stats(\n"),
            ("    def _free_blocks(self, request: Request):\n"
             "        assert request.is_finished()\n"
             "        self._free_request_blocks(request)\n",
             "    def _free_blocks(self, request: Request):\n"
             "        assert request.is_finished()\n"
             "        SPEC_RESERVATION.pop(request.request_id, None)\n"
             "        self._free_request_blocks(request)\n"),
        ],
    },
    "v1/metrics/stats.py": {
        "before_sha256": "0357ab7a4c3259119b2abc5755d3b65e29a204087eacc367b4c85a966b849506",
        "edits": [
            ("    cudagraph_stats: CUDAGraphStat | None = None\n"
             "\n"
             "    perf_stats: PerfStats | None = None\n",
             "    cudagraph_stats: CUDAGraphStat | None = None\n"
             "\n"
             "    perf_stats: PerfStats | None = None\n"
             "\n"
             "    # InferCrane patch (state by draft length): running requests by the\n"
             "    # draft tokens' state they reserved.\n"
             "    running_by_reserved_drafts: dict[int, int] = field(default_factory=dict)\n"),
        ],
    },
    "v1/metrics/loggers.py": {
        "before_sha256": "a1b927fccb4c215474255c17bd7ac37c560768664cca79ec03cddbe2089225b6",
        "edits": [
            ("logger = init_logger(__name__)\n",
             "logger = init_logger(__name__)\n"
             "\n"
             "\n"
             "# InferCrane patch (state by draft length, v4): whether this image carries the patch as\n"
             "# built, from its receipt and the patched files' sha256 (verify_state_by_draft.py).\n"
             "def _infercrane_receipt_verified() -> int:\n"
             "    try:\n"
             "        import importlib.util\n"
             "        import json\n"
             "        import pathlib\n"
             "\n"
             "        spec = importlib.util.spec_from_file_location(\n"
             "            \"infercrane_verify_state_by_draft\", \"/opt/infercrane/verify_state_by_draft.py\"\n"
             "        )\n"
             "        module = importlib.util.module_from_spec(spec)\n"
             "        spec.loader.exec_module(module)\n"
             "        receipt = json.loads(pathlib.Path(module.DEFAULT_RECEIPT).read_text())\n"
             "        problems = module.verify(receipt, pathlib.Path(__file__).resolve().parents[2])\n"
             "    except Exception:  # noqa: BLE001 - any failure reads as not verified\n"
             "        logger.warning(\"InferCrane state by draft: the receipt could not be checked\", exc_info=True)\n"
             "        return 0\n"
             "    if problems:\n"
             "        logger.warning(\"InferCrane state by draft: the receipt does not hold: %s\", problems)\n"
             "    return 0 if problems else 1\n"),
            ("        self.gauge_scheduler_waiting = create_metric_per_engine(\n"
             "            gauge_scheduler_waiting, per_engine_labelvalues\n"
             "        )\n",
             "        self.gauge_scheduler_waiting = create_metric_per_engine(\n"
             "            gauge_scheduler_waiting, per_engine_labelvalues\n"
             "        )\n"
             "\n"
             "        # InferCrane patch (state by draft length): running requests by the\n"
             "        # draft tokens' state they reserved, and the smaller of those\n"
             "        # reserving three and those reserving fewer (above zero only when\n"
             "        # both ran together).\n"
             "        self.gauge_infercrane_reserving = {\n"
             "            drafts: create_metric_per_engine(\n"
             "                self._gauge_cls(\n"
             "                    name=f\"vllm:infercrane_running_reserving_{drafts}_drafts\",\n"
             "                    documentation=f\"Running requests that reserved {drafts} draft tokens' recurrent state (InferCrane state-by-draft patch).\",\n"
             "                    multiprocess_mode=\"mostrecent\",\n"
             "                    labelnames=labelnames,\n"
             "                ),\n"
             "                per_engine_labelvalues,\n"
             "            )\n"
             "            for drafts in (0, 1, 3)\n"
             "        }\n"
             "        self.gauge_infercrane_mixed = create_metric_per_engine(\n"
             "            self._gauge_cls(\n"
             "                name=\"vllm:infercrane_running_mixed_reservations\",\n"
             "                documentation=\"The smaller of the running requests reserving three draft tokens and those reserving fewer (InferCrane state-by-draft patch).\",\n"
             "                multiprocess_mode=\"mostrecent\",\n"
             "                labelnames=labelnames,\n"
             "            ),\n"
             "            per_engine_labelvalues,\n"
             "        )\n"
             "        self.gauge_infercrane_receipt = create_metric_per_engine(\n"
             "            self._gauge_cls(\n"
             "                name=\"vllm:infercrane_state_by_draft_receipt_verified\",\n"
             "                documentation=\"1 when the image's receipt and the patched vLLM files' sha256 matched when this logger started, else 0 (InferCrane state-by-draft patch).\",\n"
             "                multiprocess_mode=\"mostrecent\",\n"
             "                labelnames=labelnames,\n"
             "            ),\n"
             "            per_engine_labelvalues,\n"
             "        )\n"
             "        infercrane_verified = _infercrane_receipt_verified()\n"
             "        for gauge in self.gauge_infercrane_receipt.values():\n"
             "            gauge.set(infercrane_verified)\n"),
            ("            self.gauge_scheduler_waiting[engine_idx].set(total_waiting)\n",
             "            self.gauge_scheduler_waiting[engine_idx].set(total_waiting)\n"
             "            # InferCrane patch (state by draft length).\n"
             "            mix = scheduler_stats.running_by_reserved_drafts\n"
             "            for drafts, gauge in self.gauge_infercrane_reserving.items():\n"
             "                gauge[engine_idx].set(mix.get(drafts, 0))\n"
             "            self.gauge_infercrane_mixed[engine_idx].set(\n"
             "                min(mix.get(3, 0), sum(count for held, count in mix.items() if held < 3))\n"
             "            )\n"),
        ],
    },
}


def digest(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


def patched(text: str, edits: list) -> str:
    for old, new in edits:
        if text.count(old) != 1:
            raise ValueError(f"expected exactly one occurrence of {old.strip().splitlines()[0]!r}")
        text = text.replace(old, new)
    return text


def after_sha256(relative: str, source: bytes) -> str:
    """The sha256 a known file has once patched; computed, so the table holds one hash per file."""
    return digest(patched(source.decode(), EDITS[relative]["edits"]).encode())


def package_dir() -> pathlib.Path:
    spec = importlib.util.find_spec("vllm")
    if spec is None or not spec.submodule_search_locations:
        raise SystemExit("the installed vLLM package is not found")
    return pathlib.Path(next(iter(spec.submodule_search_locations)))


def drop_bytecode(path: pathlib.Path) -> None:
    for cached in path.parent.glob(f"__pycache__/{path.stem}.*.pyc"):
        cached.unlink()


def restore(package: pathlib.Path, backup: pathlib.Path) -> dict:
    """Put the original files back from `backup` and check each is the released source."""
    receipt = {"schema_version": "infercrane.dev/runtime-source-patch/v1", "patch": PATCH_ID, "action": "restore", "files": []}
    for relative, entry in EDITS.items():
        saved = backup / relative
        data = saved.read_bytes()
        if digest(data) != entry["before_sha256"]:
            raise SystemExit(f"the backup of {relative} is not the released source; not restored")
        (package / relative).write_bytes(data)
        drop_bytecode(package / relative)
        found = digest((package / relative).read_bytes())
        receipt["files"].append({"file": relative, "found_sha256": found, "state": "restored" if found == entry["before_sha256"] else "not restored"})
    return receipt


def main(argv: list[str]) -> int:
    receipt_path = pathlib.Path(argv[argv.index("--receipt") + 1]) if "--receipt" in argv else None
    package = package_dir()
    if "--restore" in argv:
        receipt = restore(package, pathlib.Path(argv[argv.index("--restore") + 1]))
        text = json.dumps(receipt, indent=1)
        if receipt_path:
            receipt_path.write_text(text + "\n")
        print(text)
        return 0 if all(entry["state"] == "restored" for entry in receipt["files"]) else 1
    backup = pathlib.Path(argv[argv.index("--backup") + 1]) if "--backup" in argv else None
    import vllm

    installed = getattr(vllm, "__version__", "")
    receipt = {"schema_version": "infercrane.dev/runtime-source-patch/v1", "patch": PATCH_ID, "runtime": f"vllm-{installed}", "files": []}
    if not installed.startswith(VERSION):
        raise SystemExit(f"this patch is for vLLM {VERSION}, found {installed}")
    for relative, entry in EDITS.items():
        path = package / relative
        source = path.read_bytes()
        found = digest(source)
        record = {"file": relative, "before_sha256": entry["before_sha256"], "found_sha256": found}
        if found != entry["before_sha256"]:
            # Already patched by this patch is fine; anything else is refused.
            original = None
            record["state"] = "unknown source"
            receipt["files"].append(record)
            if "--check" not in argv:
                raise SystemExit(f"{relative} has sha256 {found}, not the vLLM {VERSION} source this patch knows; refused")
            continue
        new = patched(source.decode(), entry["edits"]).encode()
        record["after_sha256"] = digest(new)
        record["state"] = "known source"
        if "--check" not in argv:
            if backup:
                (backup / relative).parent.mkdir(parents=True, exist_ok=True)
                shutil.copyfile(path, backup / relative)
            path.write_bytes(new)
            record["state"] = "patched"
            # Drop stale bytecode so the edited source is what runs.
            drop_bytecode(path)
        receipt["files"].append(record)
    text = json.dumps(receipt, indent=1)
    if receipt_path:
        receipt_path.write_text(text + "\n")
    print(text)
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
