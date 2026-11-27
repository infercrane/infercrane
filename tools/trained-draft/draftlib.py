"""Pure helpers for the trained-draft proof of concept (no GPU, no network).

Everything here is imported by trained_draft.py inside the Modal container and
by the offline tests on a laptop. Standard library only.
"""

from __future__ import annotations

import hashlib
import json
import math
import os
import random
import re
from dataclasses import dataclass
from pathlib import Path

# ---------------------------------------------------------------------------
# Pins. A change to any of these is a change to the rules file.
# ---------------------------------------------------------------------------

MODEL = "Qwen/Qwen3-8B"
MODEL_REVISION = "b968826d9c46dd6066d109eabc6255188de91218"
PUBLISHED_DRAFT = "RedHatAI/Qwen3-8B-speculator.eagle3"
PUBLISHED_DRAFT_REVISION = "08610ffa01dd9f16731fe8f627b85905b6aa51c4"
# vLLM 0.34.0, pinned by the tag's index digest (the product path's pin, tools/poc-modal/models.py).
VLLM_IMAGE = "vllm/vllm-openai@sha256:20fdb089454afbb324efa4110a2c025ff7b27cc1a086e07aa580b18df8ad1ce1"
VLLM_VERSION = "0.34.0"
SPECULATORS_VERSION = "0.9.0"
OASST_DATASET = "OpenAssistant/oasst1"
OASST_REVISION = "fdf72ae0827c1cda404aff25b6603abec9e3399b"
OASST_FILES = {
    "train": ("data/train-00000-of-00001-b42a775f407cee45.parquet", "bbfadf5ed1278ba2208c837fdcad865adf65f5df55d80abadab2745db13fcb5e"),
    "validation": ("data/validation-00000-of-00001-134b8fd0c89408b6.parquet", "24002597bb13a7edd42d92f773762f25e285f72c31a70449393d0ded1dc7b416"),
}
# Hidden layers the draft reads (speculators' default for a 36-layer verifier: 2, n/2, n-3)
# plus the last layer, which the trainer reads as the verifier's own output
# (scripts/launch_vllm.py at speculators v0.9.0).
TARGET_LAYER_IDS = [2, 18, 33, 36]
DRAFT_TOKENS = 3
SEED = 20261126

# Modal list price for one H100 with 8 cores and 48 GiB (fast-track notes, 2026-11-23).
H100_DOLLARS_PER_HOUR = 4.7101
# One L4 with 4 cores and 32 GiB: GPU $0.000222/s (modal.com/pricing, 2026-11-25) plus cores
# and memory at the same rates as the H100 line.
L4_DOLLARS_PER_HOUR = 1.25
# One L4 with 4 cores and 64 GiB (sleeping servers keep their weights in host memory).
L4_64GIB_DOLLARS_PER_HOUR = 1.50

# Speculation each arm serves with, besides the trained draft (set per run).
NGRAM_CONFIG = {"method": "ngram", "num_speculative_tokens": 3, "prompt_lookup_min": 2, "prompt_lookup_max": 5}
# Plain suffix decoding: adaptive verification helps at 32 or more at once and trims too much
# below that on small models (vLLM PR #60339: 1.5B at 8 at once, 1.29x with, 1.47x without),
# and it needs FlashAttention 3 or Triton attention, which an L4 does not default to.
SUFFIX_CONFIG = {"method": "suffix", "num_speculative_tokens": 8}


@dataclass(frozen=True)
class Window:
    """One measurement window: servers that run at once on one GPU, the control first and last."""
    control: str
    candidates: tuple[str, ...]


@dataclass(frozen=True)
class Plan:
    """Everything a run of the ladder pins: model, drafts, GPU, sizes and money."""
    name: str
    rules: str
    model: str
    model_revision: str
    layers: int  # the verifier's hidden layers; target layers are 2, n/2, n-3 and n
    published: str
    published_revision: str
    gpu: str
    gpu_cpu: float
    gpu_memory_mib: int
    dollars_per_hour: float
    stage_seconds: dict
    train_prompts: int  # 0 for all
    generation_max_tokens: int
    generation_concurrency: int
    train_loop_seconds: int
    epochs: int
    total_seq_len: int
    memory_share: dict
    max_model_len: dict
    windows: tuple[Window, ...]
    loads: tuple[int, ...]
    requests_per_run: dict
    timing_max_tokens: int
    same_answer_prompts: int
    same_answer_max_tokens: int
    task_items: int  # 0 for none
    convert_published: bool  # the published draft is in the original EAGLE-3 format
    # Flags every server of the plan takes, training included (for example FP8 weights).
    model_args: tuple[str, ...] = ()
    # Measure servers sleep (vLLM sleep level 1) while another arm of their window runs,
    # so arms too big to share the GPU still alternate on one machine.
    sleep_mode: bool = False
    # Servers per setup that answer the task set; their item-level differences are pooled.
    task_servers: int = 1
    # How the task comparison is read: qualitycheck's RuleWithinMargin ("within_3_points") or, for
    # setups that change answers, RuleHeld ("held_within_1_point": the interval's lower end above
    # -1 point, else not shown to hold).
    task_rule: str = "within_3_points"
    # The run starts from another run's trained draft (adopt), checked by digest; the smoke serves
    # it and trains nothing.
    adopt_draft: bool = False

    @property
    def target_layer_ids(self) -> list[int]:
        return [2, self.layers // 2, self.layers - 3, self.layers]

    @property
    def precision(self) -> dict:
        """What every arm serves: FP8 weights where the plan quantizes, else BF16; the KV
        cache in the model's own dtype (no --kv-cache-dtype)."""
        return {"weights": "fp8" if "fp8" in self.model_args else "bf16", "kv_cache": "bf16"}

    def arm_args(self, arm: str, trained_dir: str) -> list[str]:
        # "none_b" is a second server without speculation: what another process alone changes.
        configs = {"none": None, "none_b": None, "ngram": NGRAM_CONFIG, "suffix": SUFFIX_CONFIG,
                   "published": {"method": "eagle3", "model": self.published, "revision": self.published_revision, "num_speculative_tokens": DRAFT_TOKENS},
                   "trained": {"method": "eagle3", "model": trained_dir, "num_speculative_tokens": DRAFT_TOKENS}}
        config = configs[arm]
        return [] if config is None else ["--speculative-config", json.dumps(config, separators=(",", ":"))]


PLANS = {
    # Rung 2: a small model, every drafting method, one L4.
    "l4-qwen3-1.7b": Plan(
        name="l4-qwen3-1.7b", rules="rules-rung2-l4-qwen3-1.7b-2026-11-26.md", model="Qwen/Qwen3-1.7B", model_revision="70d244cc86ccca08cf5af4e1e306ecf908b1ad5e", layers=28,
        published="AngelSlim/Qwen3-1.7B_eagle3", published_revision="94441b48acc5804677ae12259617c83323b543a9", gpu="L4", gpu_cpu=4.0, gpu_memory_mib=32768,
        dollars_per_hour=L4_DOLLARS_PER_HOUR, stage_seconds={"smoke": 25 * 60, "train": 50 * 60, "measure": 60 * 60, "confirm": 20 * 60}, train_prompts=4000,
        generation_max_tokens=512, generation_concurrency=128, train_loop_seconds=20 * 60, epochs=2, total_seq_len=4096,
        memory_share={"generate": 0.85, "extract": 0.30, "measure": 0.30}, max_model_len={"generate": 4096, "extract": 4096, "measure": 2048},
        # The drafts' window first: the pass bar reads it; the model-free window is reported only.
        windows=(Window("none", ("published", "trained")), Window("none", ("ngram", "suffix"))), loads=(1, 8), requests_per_run={1: 8, 8: 32},
        timing_max_tokens=256, same_answer_prompts=128, same_answer_max_tokens=256, task_items=0, convert_published=True),
    # Rung 3: Qwen3-8B end to end on one L4. FP8 weights for every server (the L4 setup step 2
    # measured best), training included, so the draft learns the hidden states it is served with.
    # Three arms cannot share 24 GB at once, so they alternate through vLLM sleep mode.
    "l4-qwen3-8b": Plan(
        name="l4-qwen3-8b", rules="rules-rung3-l4-qwen3-8b-2026-11-26.md", model=MODEL, model_revision=MODEL_REVISION, layers=36,
        published=PUBLISHED_DRAFT, published_revision=PUBLISHED_DRAFT_REVISION, gpu="L4", gpu_cpu=4.0, gpu_memory_mib=65536,
        dollars_per_hour=L4_64GIB_DOLLARS_PER_HOUR, stage_seconds={"smoke": 30 * 60, "train": 60 * 60, "measure": 100 * 60, "confirm": 35 * 60}, train_prompts=2000,
        generation_max_tokens=512, generation_concurrency=64, train_loop_seconds=25 * 60, epochs=2, total_seq_len=2048,
        # The extraction server shares the GPU with the trainer: FP8 weights 9.06 GiB, activations
        # about 1.7, and cache for one row of total_seq_len need about 0.49 of an L4 (r3-2's smoke).
        memory_share={"generate": 0.85, "extract": 0.55, "measure": 0.72}, max_model_len={"generate": 4096, "extract": 2304, "measure": 4096},
        windows=(Window("none", ("published", "trained")),), loads=(1, 32), requests_per_run={1: 8, 32: 96},
        timing_max_tokens=256, same_answer_prompts=128, same_answer_max_tokens=256, task_items=500, convert_published=False,
        model_args=("--quantization", "fp8"), sleep_mode=True),
    # Rung 4: rung 3's trained draft (adopted by digest, no training) on one H100 with the same FP8
    # weights it was trained against, so only the GPU changes. A second server without speculation
    # (none_b) measures what another process alone changes, in speed and in greedy text, and three
    # servers of each setup answer the task set, read by the product's rule for setups that change
    # answers (held only when the interval's lower end is above -1 point).
    "h100-qwen3-8b-fp8": Plan(
        name="h100-qwen3-8b-fp8", rules="rules-rung4-h100-qwen3-8b-2026-11-26.md", model=MODEL, model_revision=MODEL_REVISION, layers=36,
        published=PUBLISHED_DRAFT, published_revision=PUBLISHED_DRAFT_REVISION, gpu="H100", gpu_cpu=8.0, gpu_memory_mib=65536,
        dollars_per_hour=H100_DOLLARS_PER_HOUR, stage_seconds={"smoke": 15 * 60, "train": 30 * 60, "measure": 60 * 60, "confirm": 15 * 60}, train_prompts=2000,
        generation_max_tokens=512, generation_concurrency=64, train_loop_seconds=20 * 60, epochs=2, total_seq_len=2048,
        memory_share={"generate": 0.85, "extract": 0.40, "measure": 0.22}, max_model_len={"generate": 4096, "extract": 2304, "measure": 4096},
        windows=(Window("none", ("published", "trained", "none_b")),), loads=(1, 32), requests_per_run={1: 8, 32: 96},
        timing_max_tokens=256, same_answer_prompts=128, same_answer_max_tokens=256, task_items=500, convert_published=False,
        model_args=("--quantization", "fp8"), task_servers=3, task_rule="held_within_1_point", adopt_draft=True),
}
PLAN_VARIABLE = "TRAINED_DRAFT_PLAN"


def plan_for(name: str | None = None) -> Plan:
    return PLANS[name or os.environ.get(PLAN_VARIABLE, "l4-qwen3-1.7b")]

REPORT_SCHEMA = "infercrane.dev/draft-training-report/v1"
# What a vLLM 0.34 server of each model needs, read from the servers' own logs, so a plan's memory
# shares are checked before any GPU starts (test_every_server_fits_its_memory_share). Weights:
# "Model loading took"; activations: the share's memory less the weights and the "Available KV
# cache memory" a server reported (r2-3's servers for Qwen3-1.7B, r3-2's failed extraction server
# for Qwen3-8B in FP8); cache per token: layers x KV heads x head size x 2 (keys, values) x 2 bytes.
# Qwen3-8B in BF16 (rung 4) is unmeasured: its weights are the checkpoint's file size.
SERVER_MEMORY = {
    "Qwen/Qwen3-1.7B": {"weights_gib": {"bf16": 3.22}, "draft_gib": 0.25, "activations_gib": 1.1, "cache_bytes_per_token": 28 * 8 * 128 * 2 * 2},
    "Qwen/Qwen3-8B": {"weights_gib": {"fp8": 9.06, "bf16": 15.3}, "draft_gib": 1.0, "activations_gib": 1.71, "cache_bytes_per_token": 36 * 8 * 128 * 2 * 2},
}
# Memory a CUDA program can use on each GPU (nvidia-smi's total, as the stages record it).
GPU_GIB = {"L4": 23034 / 1024, "H100": 81559 / 1024}


def server_headroom_gib(plan: "Plan", role: str) -> float:
    """What a server of the plan has left for its cache after its weights (with a draft when
    measuring) and activations, less the cache one full-length sequence needs. Negative: it will
    not start."""
    facts = SERVER_MEMORY[plan.model]
    weights = facts["weights_gib"][plan.precision["weights"]] + (facts["draft_gib"] if role == "measure" else 0.0)
    one_sequence = facts["cache_bytes_per_token"] * plan.max_model_len[role] / 2**30
    return plan.memory_share[role] * GPU_GIB[plan.gpu] - weights - facts["activations_gib"] - one_sequence


RECORD_SCHEMA = "infercrane.trained-draft-measure/v2"
# The confirm stage's record: the measure record's shape, one window of the trained draft against
# the control, timed runs only, from a container other than the measure stage's.
CONFIRM_SCHEMA = "infercrane.trained-draft-confirm/v1"
CONFIRM_FILE = "confirm.json"

# ---------------------------------------------------------------------------
# Prompts from oasst1: first turns of conversation trees, train and validation
# splits as the dataset publishes them (disjoint trees).
# ---------------------------------------------------------------------------

MAX_PROMPT_CHARS = 4000


def root_prompts(rows: list[dict]) -> list[dict]:
    """First user turns of the dataset's conversation trees, deduplicated by text.

    A row is kept when it opens a tree (no parent), was written by a person
    (role prompter, not synthetic), is not deleted, passed review when reviewed,
    and is at most MAX_PROMPT_CHARS long. Order is by message id, so the result
    does not depend on the file's row order.
    """
    seen: set[str] = set()
    out = []
    for row in sorted(rows, key=lambda r: str(r.get("message_id"))):
        text = (row.get("text") or "").strip()
        if row.get("parent_id") or row.get("role") != "prompter" or row.get("deleted") or row.get("synthetic"):
            continue
        if row.get("review_result") is False or not text or len(text) > MAX_PROMPT_CHARS or text in seen:
            continue
        seen.add(text)
        out.append({"id": f"oasst1:{row['message_id']}", "tree": str(row.get("message_tree_id") or row["message_id"]), "lang": row.get("lang") or "", "content": text})
    return out


def split_prompts(train_rows: list[dict], validation_rows: list[dict]) -> dict:
    """Training prompts from the train split, held-out prompts from the
    validation split. A held-out prompt whose text or tree also appears in
    training is dropped, so no held-out prompt was trained on."""
    train = root_prompts(train_rows)
    trained_text = {p["content"] for p in train}
    trained_trees = {p["tree"] for p in train}
    held_out = [p for p in root_prompts(validation_rows) if p["content"] not in trained_text and p["tree"] not in trained_trees]
    rng = random.Random(SEED)
    rng.shuffle(held_out)
    return {"train": train, "held_out": held_out}


def digest_lines(items: list[dict]) -> str:
    """sha256 of the sorted per-item digests: tells two sets apart without holding them."""
    item_digests = sorted(hashlib.sha256(json.dumps(item, sort_keys=True, ensure_ascii=False).encode()).hexdigest() for item in items)
    return "sha256:" + hashlib.sha256("\n".join(item_digests).encode()).hexdigest()


def languages(items: list[dict]) -> dict[str, int]:
    counts: dict[str, int] = {}
    for item in items:
        counts[item.get("lang") or "unknown"] = counts.get(item.get("lang") or "unknown", 0) + 1
    return dict(sorted(counts.items(), key=lambda kv: (-kv[1], kv[0])))


# ---------------------------------------------------------------------------
# Requests and training rows.
# ---------------------------------------------------------------------------


def chat_request(model: str, content: str, max_tokens: int, *, stream: bool, token_ids: bool = False) -> dict:
    """One chat request as every arm receives it: one user message, temperature 0,
    thinking off (the corpus's rule), and the usage chunk when streamed."""
    body: dict = {
        "model": model, "messages": [{"role": "user", "content": content}], "max_tokens": max_tokens,
        "temperature": 0.0, "chat_template_kwargs": {"enable_thinking": False}, "stream": stream,
    }
    if stream:
        body["stream_options"] = {"include_usage": True}
    if token_ids:
        body["return_token_ids"] = True
    return body


def training_row(prompt_id: str, response: dict) -> dict | None:
    """A speculators training row from one answer: the prompt's token ids with
    loss mask 0 and the answer's with 1, exactly as vLLM tokenized and wrote
    them (speculators' build_boundary_sample). None when the answer is empty."""
    prompt_ids = response.get("prompt_token_ids") or []
    choice = (response.get("choices") or [{}])[0]
    answer_ids = choice.get("token_ids") or []
    if not prompt_ids or not answer_ids:
        return None
    return {"id": prompt_id, "input_ids": list(prompt_ids) + list(answer_ids), "loss_mask": [0] * len(prompt_ids) + [1] * len(answer_ids)}


# ---------------------------------------------------------------------------
# The measurement window: the campaign's interleaving with one control.
# ---------------------------------------------------------------------------


def window_schedule(candidates: list[str], runs_per_candidate: int) -> list[str]:
    """control, c1, control, c2, ..., control: every candidate run sits between two
    control runs, candidates take turns, and the window starts and ends on the
    control (internal/optimizationcampaign comparePaired's shape)."""
    order = ["control"]
    for _ in range(runs_per_candidate):
        for candidate in candidates:
            order += [candidate, "control"]
    return order


def percentile(values: list[float], fraction: float) -> float | None:
    if not values:
        return None
    ordered = sorted(values)
    rank = fraction * (len(ordered) - 1)
    low, high = math.floor(rank), math.ceil(rank)
    return ordered[low] + (ordered[high] - ordered[low]) * (rank - low)


def summarize_run(arm: str, results: list[dict], duration: float) -> dict:
    """A run summary in the judge's RunSummary shape (internal/optimizationcampaign).
    Each result has ok, ttft, latency (seconds) and output_tokens. A value no
    request reported is absent, never zero."""
    ok = [r for r in results if r.get("ok")]
    tokens = sum(int(r.get("output_tokens") or 0) for r in ok)
    summary: dict = {"arm": arm, "requests": len(results), "failed": len(results) - len(ok), "duration_seconds": round(duration, 4)}
    if duration > 0 and ok:
        summary["output_token_throughput"] = tokens / duration
        summary["request_throughput"] = len(ok) / duration
        summary["output_tokens_per_request"] = tokens / len(ok)
    ttft = [r["ttft"] * 1000 for r in ok if r.get("ttft") is not None]
    latency = [r["latency"] * 1000 for r in ok if r.get("latency") is not None]
    tpot = [(r["latency"] - r["ttft"]) * 1000 / (r["output_tokens"] - 1) for r in ok if r.get("ttft") is not None and (r.get("output_tokens") or 0) > 1]
    for key, values in (("ttft_p95_ms", ttft), ("tpot_p95_ms", tpot), ("latency_p95_ms", latency)):
        value = percentile(values, 0.95)
        if value is not None:
            summary[key] = value
    return summary


# ---------------------------------------------------------------------------
# Speculation counters from vLLM's /metrics.
# ---------------------------------------------------------------------------

_METRIC = re.compile(r'^(vllm:spec_decode_num_(?:drafts|draft_tokens|accepted_tokens)(?:_per_pos)?_total)(\{[^}]*\})?\s+([0-9.eE+-]+)$')
_POSITION = re.compile(r'position="(\d+)"')


def speculation_counters(text: str) -> dict:
    """Totals of vLLM's speculation counters, summed over label sets except position."""
    out: dict = {"drafts": 0.0, "draft_tokens": 0.0, "accepted_tokens": 0.0, "accepted_per_position": {}}
    for line in text.splitlines():
        match = _METRIC.match(line.strip())
        if not match:
            continue
        name, labels, value = match.group(1), match.group(2) or "", float(match.group(3))
        if name.endswith("_per_pos_total"):
            position = _POSITION.search(labels)
            if position:
                index = int(position.group(1))
                out["accepted_per_position"][index] = out["accepted_per_position"].get(index, 0.0) + value
        elif name == "vllm:spec_decode_num_drafts_total":
            out["drafts"] += value
        elif name == "vllm:spec_decode_num_draft_tokens_total":
            out["draft_tokens"] += value
        elif name == "vllm:spec_decode_num_accepted_tokens_total":
            out["accepted_tokens"] += value
    return out


def acceptance(before: dict, after: dict) -> dict | None:
    """Acceptance between two counter readings: mean tokens a verify step writes
    (1 + accepted per draft) and, per position, the share accepted given every
    earlier position was (the training report's convention). None without drafts."""
    drafts = after["drafts"] - before["drafts"]
    if drafts <= 0:
        return None
    accepted = after["accepted_tokens"] - before["accepted_tokens"]
    unconditional = []
    for position in sorted(after["accepted_per_position"]):
        unconditional.append((after["accepted_per_position"][position] - before["accepted_per_position"].get(position, 0.0)) / drafts)
    conditional, previous = [], 1.0
    for share in unconditional:
        conditional.append(round(min(1.0, share / previous), 6) if previous > 0 else 0.0)
        previous = share
    return {"drafts": drafts, "mean_tokens_per_step": round(1 + accepted / drafts, 6), "per_position": conditional}


# ---------------------------------------------------------------------------
# The draft's files and the training report (internal/speculator Report).
# ---------------------------------------------------------------------------


def file_manifest(directory: Path) -> tuple[list[dict], str]:
    """Every file under directory with its size and sha256, and the manifest
    digest computed exactly as speculator.ManifestDigest computes it."""
    files = []
    for path in sorted(p for p in directory.rglob("*") if p.is_file()):
        digest = hashlib.sha256()
        with path.open("rb") as handle:
            for chunk in iter(lambda: handle.read(1 << 20), b""):
                digest.update(chunk)
        files.append({"path": path.relative_to(directory).as_posix(), "size": path.stat().st_size, "sha256": digest.hexdigest()})
    return files, manifest_digest(files)


def manifest_digest(files: list[dict]) -> str:
    lines = "".join(f"{f['path']} {f['size']} {f['sha256']}\n" for f in sorted(files, key=lambda f: f["path"]))
    return "sha256:" + hashlib.sha256(lines.encode()).hexdigest()


@dataclass
class TrainingFacts:
    name: str
    examples: int
    held_out_examples: int
    training_tokens: int
    dataset_fingerprint: str
    held_out_acceptance: list[float]
    start_acceptance: list[float]
    produced_at: str
    wall_seconds: float


def training_report(facts: TrainingFacts, draft_dir: Path, plan: Plan | None = None) -> dict:
    """The numbers-only report a training job leaves beside the draft
    (speculator.ReportSchema). Validated by the core's DecodeReport in the judge."""
    plan = plan or plan_for()
    files, digest = file_manifest(draft_dir)
    return {
        "schema": REPORT_SCHEMA, "name": facts.name, "base_model": plan.model, "base_revision": plan.model_revision, "method": "eagle3",
        "start_from": {"repository": plan.published, "revision": plan.published_revision}, "draft_tokens": DRAFT_TOKENS,
        "framework": "speculators", "framework_version": SPECULATORS_VERSION, "data_source": "public_dataset",
        "dataset_fingerprint": facts.dataset_fingerprint, "examples": facts.examples, "held_out_examples": facts.held_out_examples,
        "training_tokens": facts.training_tokens, "held_out_acceptance": facts.held_out_acceptance, "start_acceptance": facts.start_acceptance,
        "produced_at": facts.produced_at, "files": files, "artifact_digest": digest, "wall_seconds": round(facts.wall_seconds, 3),
    }


# ---------------------------------------------------------------------------
# Money.
# ---------------------------------------------------------------------------


def dollars(seconds: float, rate_per_hour: float = H100_DOLLARS_PER_HOUR) -> float:  # noqa: D103
    return round(seconds / 3600 * rate_per_hour, 2)


# ---------------------------------------------------------------------------
# Evidence beside the measure record: what the contract record digests.
# ---------------------------------------------------------------------------

ACCEPTANCE_SCHEMA = "infercrane.draft-acceptance/v1"
SHAPE_SCHEMA = "infercrane.trained-draft-traffic-shape/v1"
ACCEPTANCE_FILE = "acceptance.json"
SHAPE_FILE = "traffic-shape.json"
SPECULATION_COUNTERS = ("vllm:spec_decode_num_drafts_total", "vllm:spec_decode_num_draft_tokens_total",
                        "vllm:spec_decode_num_accepted_tokens_total", "vllm:spec_decode_num_accepted_tokens_per_pos_total")


def acceptance_evidence(record: dict) -> dict:
    """The draft-acceptance evidence of a measure record (the technique's manifest requires
    it): for every drafting arm, vLLM's speculation counters read before and after the timed
    runs of each load and around the identical-answer pass, and the acceptance derived from
    them. Counts only, no text."""
    facts = {key: record.get(key) for key in ("model", "model_revision", "engine", "image", "gpu", "draft_tokens")}
    return {"schema": ACCEPTANCE_SCHEMA, "source": "vLLM /metrics", "counters": list(SPECULATION_COUNTERS), **facts,
            "windows": [{"control": window.get("control"), "candidates": window.get("candidates", []),
                         "by_load": window.get("acceptance", {}), "readings_by_load": window.get("acceptance_readings", {}),
                         "held_out": window.get("held_out_acceptance", {}), "held_out_readings": window.get("held_out_readings", {})}
                        for window in record.get("windows", [])]}


def traffic_shape(record: dict) -> dict:
    """The content-free traffic shape of a measure record: which held-out oasst1 prompts, by
    id, every load sent and in what order, how many at once, and the answer caps. Ids of a
    public dataset and counts only, no text."""
    windows = []
    for window in record.get("windows", []):
        same = window.get("same_answers") or {}
        windows.append({"control": window.get("control"),
                        "loads": [{key: level.get(key) for key in ("concurrency", "requests_per_run", "max_tokens", "warm_ids", "prompt_ids")} for level in window.get("loads", [])],
                        "same_answers": {"ids": same.get("ids", []), "max_tokens": same.get("max_tokens")},
                        "task_items": [item["id"] for item in (window.get("tasks") or {}).get("items", [])]})
    return {"schema": SHAPE_SCHEMA, "dataset": OASST_DATASET, "dataset_revision": OASST_REVISION, "split": "validation",
            "request": {"temperature": 0.0, "thinking": False, "stream": True}, "windows": windows}


def write_evidence(run_dir: Path, record: dict) -> dict:
    """Writes both evidence files and returns each one's sha256, as the judge digests them."""
    out = {}
    for name, body in ((ACCEPTANCE_FILE, acceptance_evidence(record)), (SHAPE_FILE, traffic_shape(record))):
        write_json(run_dir / name, body)
        out[name] = "sha256:" + hashlib.sha256((run_dir / name).read_bytes()).hexdigest()
    return out


def confirm_refusal(verdict: dict) -> str:
    """Why the confirm stage may not run on a judged measure record, or "" when it may: the
    measurement was valid, answers held (the task rule where the plan has a task set), and the
    trained draft was faster than the window's control at one load or more."""
    bar = verdict.get("pass_bar") or {}
    if not str(bar.get("bar_validity", "")).startswith("pass"):
        return "the measure stage was not valid: " + str(bar.get("bar_validity", "no validity line"))
    if not str(bar.get("bar_answers_held", "")).startswith("pass"):
        return "answers did not hold: " + str(bar.get("bar_answers_held", "no answers line"))
    faster = sorted(key for key, value in bar.items() if key.startswith("trained_vs_") and str(value).startswith("faster"))
    if not faster:
        return "the trained draft was faster than the control at no load"
    return ""


def write_json(path: Path, value) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    temporary = path.with_suffix(path.suffix + ".part")
    temporary.write_text(json.dumps(value, ensure_ascii=False, indent=1, sort_keys=True))
    os.replace(temporary, path)
