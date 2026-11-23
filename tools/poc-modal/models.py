"""The models the Modal GPU PoC can serve, their publishers' commands, and the vLLM
arguments a remote server accepts. No Modal import: the laptop driver (poc_modal.py)
plans with the same table the GPU side (modal_gpu.py) enforces."""

from __future__ import annotations

import json
import os
from dataclasses import dataclass, field


@dataclass(frozen=True)
class Engine:
    """A serving engine image a run pins by digest. Every setup of a run uses one."""

    key: str
    version: str
    image: str

    @property
    def label(self) -> str:
        """The runtime version the control plane records for a setup."""
        return f"vllm-openai-{self.version}-{self.image.rsplit(':', 1)[1][:8]}"


@dataclass(frozen=True)
class Accelerator:
    """A GPU type Modal can pin. usd_per_second is Modal's list price for the GPU alone."""

    key: str
    modal_gpu: str  # "!" pins the type; Modal may not substitute a larger one
    usd_per_second: float
    description: str
    price_read: str


# vLLM 0.22.1 is the version the product pins today (the same image as
# tools/modal-kernel-poc on PR #229). 0.34.0 was the newest release on 2026-11-23
# (hub.docker.com/r/vllm/vllm-openai, tag v0.34.0, pushed 2026-11-16; `latest` named the
# same digest). Runs meant for publication use the newest engine, so the baseline is not
# a months-old server.
ENGINES = {engine.key: engine for engine in (
    Engine("vllm-0.22.1", "0.22.1", "vllm/vllm-openai@sha256:953d3a06d5e64ab582985cd7401289d3abf2a2c14ef2158e9a84313daeec77d7"),
    Engine("vllm-0.34.0", "0.34.0", "vllm/vllm-openai@sha256:20fdb089454afbb324efa4110a2c025ff7b27cc1a086e07aa580b18df8ad1ce1"),
)}
# Modal list prices, GPU only (modal.com/pricing): H100 read 2026-11-20, H200 read
# 2026-11-23 as $4.54 an hour. The machine-difference allowance in the product's judge
# was measured on H100s only.
ACCELERATORS = {accelerator.key: accelerator for accelerator in (
    Accelerator("H100", "H100!", 0.001097, "NVIDIA H100 SXM5 80GB", "2026-11-20"),
    Accelerator("H200", "H200!", round(4.54 / 3600, 6), "NVIDIA H200 141GB", "2026-11-23"),
)}
ENGINE_VARIABLE, ACCELERATOR_VARIABLE = "INFERCRANE_POC_ENGINE", "INFERCRANE_POC_GPU"
# The run's engine and GPU type. The laptop driver, the broker and every remote
# container must agree, so both come from the environment: the driver sets it for the
# broker it starts, and the Modal image carries it into the containers.
ENGINE = ENGINES[os.environ.get(ENGINE_VARIABLE, "vllm-0.22.1")]
ACCELERATOR = ACCELERATORS[os.environ.get(ACCELERATOR_VARIABLE, "H100")]


def select(engine: str, accelerator: str) -> None:
    """Sets the run's engine and GPU type for this process and the processes it starts."""
    global ENGINE, ACCELERATOR
    if engine not in ENGINES or accelerator not in ACCELERATORS:
        raise ValueError(f"engine must be one of {', '.join(ENGINES)} and the GPU one of {', '.join(ACCELERATORS)}")
    ENGINE, ACCELERATOR = ENGINES[engine], ACCELERATORS[accelerator]
    os.environ[ENGINE_VARIABLE], os.environ[ACCELERATOR_VARIABLE] = engine, accelerator


@dataclass(frozen=True)
class EngineSetup:
    """What one engine version needs to serve a model's publisher command."""

    # The environment every setup runs with.
    env: dict = field(default_factory=dict)
    # Arguments added to the publisher's because its command does not start on this
    # engine as it is, and why (also for an environment that differs from the
    # publisher's). The current setup is the publisher's command plus these.
    added_args: tuple = ()
    added_because: str = ""
    # The most requests vLLM can run at once on one H100 with this setup, when the model
    # has a hard limit; a planned setup above it cannot start.
    max_num_seqs: int | None = None
    # The date of the serving check on a real GPU that showed the command starts and
    # serves; empty until there is one.
    checked: str = ""


@dataclass(frozen=True)
class Model:
    """A model the remote volume holds. Every setup of a run serves the same model with
    the same environment; setups differ only in their vLLM arguments."""

    id: str
    revision: str
    # The publisher's own vLLM arguments and environment for one H100, after the model.
    publisher_args: tuple
    source: str
    publisher_env: dict | None = None
    # What each engine version needs on top; an engine without an entry serves the
    # publisher's arguments with no added environment.
    setups: dict = field(default_factory=dict)
    # The speculation option the publisher documents for this model, as vLLM arguments,
    # and where it says so. Empty when it documents none this harness can serve.
    publisher_speculation: tuple = ()
    speculation_source: str = ""

    @property
    def dir(self) -> str:
        return f"/weights/{self.id.split('/', 1)[1].lower()}-{self.revision[:12]}"

    @property
    def setup(self) -> EngineSetup:
        return self.setups.get(ENGINE.key, EngineSetup())

    @property
    def env(self) -> dict:
        return self.setup.env

    @property
    def added_args(self) -> tuple:
        return self.setup.added_args

    @property
    def added_because(self) -> str:
        return self.setup.added_because

    @property
    def max_num_seqs(self) -> int | None:
        return self.setup.max_num_seqs

    @property
    def current_args(self) -> list[str]:
        return [*self.publisher_args, *self.added_args]

    @property
    def speculation_args(self) -> list[str]:
        """The publisher's command with its documented speculation option."""
        return [*self.current_args, *self.publisher_speculation]


# vLLM 0.22.1 can mis-detect its vendored DeepGEMM as usable on H100 and fail while
# warming a BF16 model (tools/modal-kernel-poc), so the BF16 model runs without it.
QWEN3_8B = Model("Qwen/Qwen3-8B", "b968826d9c46dd6066d109eabc6255188de91218", (),
                 "vLLM defaults, the current setup of the 2026-11-20 campaign",
                 setups={"vllm-0.22.1": EngineSetup(env={"VLLM_USE_DEEP_GEMM": "0"}, checked="2026-11-20")})
# The most downloaded FP8 checkpoint from Qwen, Z.ai and DeepSeek that fits one H100 on
# 2026-11-21 (poc_modal.py choose-model). Its command is vLLM's own recipe for one H100 in FP8,
# the page the model card links; the recipe needs vLLM 0.17 or later and transformers
# 5.8.0 or later, and the pinned image has vLLM 0.22.1 and transformers 5.10.2.
QWEN38_27B_FP8 = Model("Qwen/Qwen3.8-27B-FP8", "c3a462cc3d38cf351f3b26cbd763469eeb74ef81",
                       ("--tensor-parallel-size", "1", "--enable-auto-tool-choice", "--tool-call-parser", "qwen3_coder",
                        "--reasoning-parser", "qwen3", "--mm-encoder-tp-mode", "data"),
                       "https://recipes.vllm.ai/Qwen/Qwen3.8-27B.json?hardware=h100&variant=fp8&strategy=single_node_tp (read 2026-11-21, unchanged 2026-11-23)",
                       publisher_env={"VLLM_USE_RUST_FRONTEND": "1"},
                       setups={
                           # 48 of its 64 layers keep a recurrent state, one cache block per
                           # running request. On one H100 (2026-11-21 check) vLLM 0.22.1 had 818
                           # such blocks and refused to start with its default of 1,024 requests
                           # at once. The campaign runs 32 at once, so 512 changes nothing it
                           # measures. The publisher sets VLLM_USE_RUST_FRONTEND=1. In this image
                           # its Rust HTTP frontend exited with code 1 once the engine was ready
                           # (2026-11-21 check), so every setup uses vLLM's Python frontend, which
                           # the recipe also offers for images without the Rust one.
                           "vllm-0.22.1": EngineSetup(
                               added_args=("--max-num-seqs", "512"),
                               added_because=("vLLM 0.22.1 does not start the publisher's command on one H100: max_num_seqs 1024 exceeds the 818 recurrent-state cache blocks, "
                                              "and vLLM asks for at most 818. Its Rust frontend (VLLM_USE_RUST_FRONTEND=1) exited at startup, so the Python frontend serves."),
                               max_num_seqs=818, checked="2026-11-21"),
                           # The publisher's command and environment as published. Not yet
                           # checked on a GPU: run `modal_gpu.py::main --action check` first.
                           "vllm-0.34.0": EngineSetup(env={"VLLM_USE_RUST_FRONTEND": "1"}),
                       },
                       # The recipe page's own line: "Add --speculative-config
                       # '{"method":"mtp","num_speculative_tokens":3}' for MTP." It also documents
                       # two external drafters (DFlash2, DSpark) that serve the BF16 checkpoint
                       # with separate draft weights; this harness stages neither.
                       publisher_speculation=("--speculative-config", '{"method":"mtp","num_speculative_tokens":3}'),
                       speculation_source="https://recipes.vllm.ai/Qwen/Qwen3.8-27B.md?hardware=h100&variant=fp8&strategy=single_node_tp (read 2026-11-23)")
MODELS = {model.id: model for model in (QWEN3_8B, QWEN38_27B_FP8)}


# Arguments a pod may pass to vLLM: the publishers' commands and the single-GPU changes the
# planner's diagnosis rules make. Anything else is refused, so a candidate can never ask
# for more GPUs, another port, or another model. Speculative decoding is accepted only with
# the model's own prediction head (no draft weights are staged). Whether a run may measure
# it is the driver's decision: it needs traffic with real text (poc_modal.py plan).
VALUE_FLAGS = {"--speculative-config", "--served-model-name", "--max-model-len", "--quantization", "--kv-cache-dtype", "--max-num-batched-tokens",
               "--max-num-seqs", "--gpu-memory-utilization", "--block-size", "--seed", "--dtype", "--tensor-parallel-size",
               "--tool-call-parser", "--reasoning-parser", "--mm-encoder-tp-mode", "--api-server-count", "--stream-interval",
               "--max-cudagraph-capture-size"}
SWITCH_FLAGS = {"--enable-prefix-caching", "--no-enable-prefix-caching", "--enable-chunked-prefill", "--no-enable-chunked-prefill",
                "--async-scheduling", "--no-async-scheduling", "--calculate-kv-scales", "--enable-auto-tool-choice"}
# Values a flag must keep: one GPU per server, and a bounded number of API processes.
FIXED_VALUES = {"--tensor-parallel-size": {"1"}, "--mm-encoder-tp-mode": {"data", "weights"}, "--api-server-count": {"1", "2", "3", "4"}}
MAX_SPECULATIVE_TOKENS = 8


def speculation(value: str) -> dict:
    """The speculation a --speculative-config value asks for; raises ValueError unless it
    is the model's own prediction head with a bounded number of drafted tokens."""
    try:
        config = json.loads(value)
    except json.JSONDecodeError as error:
        raise ValueError("--speculative-config must be a JSON object") from error
    tokens = config.get("num_speculative_tokens") if isinstance(config, dict) else None
    if not isinstance(config, dict) or set(config) != {"method", "num_speculative_tokens"} or config["method"] != "mtp" \
            or isinstance(tokens, bool) or not isinstance(tokens, int) or not 1 <= tokens <= MAX_SPECULATIVE_TOKENS:
        raise ValueError(f"only speculation with the model's own prediction head is allowed on the remote GPU: method mtp with 1 to {MAX_SPECULATIVE_TOKENS} tokens and no draft model")
    return config


def speculates(args: list[str] | tuple) -> bool:
    return any(flag.startswith("--speculative") for flag, _ in parse(args))


def check_args(args: list[str], model: str) -> None:
    """Refuses arguments outside the allowlist; raises ValueError."""
    if model not in MODELS:
        raise ValueError(f"only {', '.join(MODELS)} can be staged on the remote volume, not {model}")
    index, served = 0, None
    while index < len(args):
        flag = args[index]
        name = flag.split("=", 1)[0]
        if name in SWITCH_FLAGS and "=" not in flag:
            index += 1
            continue
        if name not in VALUE_FLAGS:
            raise ValueError(f"vLLM argument {flag!r} is not allowed on the remote GPU")
        if "=" in flag:
            value = flag.split("=", 1)[1]
            index += 1
        else:
            if index + 1 >= len(args) or args[index + 1].startswith("--"):
                raise ValueError(f"vLLM argument {flag} needs a value")
            value = args[index + 1]
            index += 2
        if name in FIXED_VALUES and value not in FIXED_VALUES[name]:
            raise ValueError(f"vLLM argument {name} {value} is not allowed on the remote GPU")
        if name == "--speculative-config":
            speculation(value)
        if name == "--served-model-name":
            served = value
    if served != model:
        raise ValueError("--served-model-name must be the lease's model")


def parse(args: list[str] | tuple) -> list[tuple[str, str | None]]:
    """vLLM arguments as (flag, value) pairs; a switch has no value."""
    pairs, index = [], 0
    while index < len(args):
        flag = args[index]
        if "=" in flag:
            name, value = flag.split("=", 1)
            pairs.append((name, value))
            index += 1
        elif index + 1 < len(args) and not args[index + 1].startswith("--"):
            pairs.append((flag, args[index + 1]))
            index += 2
        else:
            pairs.append((flag, None))
            index += 1
    return pairs


def flatten(pairs: list[tuple[str, str | None]]) -> list[str]:
    return [token for flag, value in pairs for token in ((flag,) if value is None else (flag, value))]


def with_change(current: list[str] | tuple, start: list[str], changed: list[str]) -> list[str]:
    """Applies to current the change that turns start into changed: flags changed sets or
    replaces, and flags it removed from start are removed. The planner's setups are its
    starting setup with one change, so this carries that one change onto another command."""
    before, after = dict(parse(start)), parse(changed)
    removed = {flag for flag in before if flag not in dict(after)}
    added = [(flag, value) for flag, value in after if flag not in before or before[flag] != value]
    pairs = [(flag, value) for flag, value in parse(current) if flag not in removed]
    for flag, value in added:
        position = next((index for index, (name, _) in enumerate(pairs) if name == flag), None)
        if position is None:
            pairs.append((flag, value))
        else:
            pairs[position] = (flag, value)
    return flatten(pairs)
