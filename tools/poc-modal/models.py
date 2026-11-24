"""The models the Modal GPU PoC can serve, their publishers' commands, and the vLLM
arguments a remote server accepts. No Modal import: the laptop driver (poc_modal.py)
plans with the same table the GPU side (modal_gpu.py) enforces."""

from __future__ import annotations

from dataclasses import dataclass


@dataclass(frozen=True)
class Model:
    """A model the remote volume holds. Every setup of a run serves the same model with
    the same environment; setups differ only in their vLLM arguments."""

    id: str
    revision: str
    # The environment every setup runs with.
    env: dict
    # The publisher's own vLLM arguments and environment for one H100, after the model.
    publisher_args: tuple
    source: str
    publisher_env: dict | None = None
    # Arguments added to the publisher's because its command does not start on the pinned
    # image as it is, and why (also for an environment that differs from the publisher's).
    # The current setup is the publisher's command plus these.
    added_args: tuple = ()
    added_because: str = ""
    # The most requests vLLM can run at once on one H100 with this setup, when the model
    # has a hard limit; a planned setup above it cannot start.
    max_num_seqs: int | None = None

    @property
    def dir(self) -> str:
        return f"/weights/{self.id.split('/', 1)[1].lower()}-{self.revision[:12]}"

    @property
    def current_args(self) -> list[str]:
        return [*self.publisher_args, *self.added_args]


# vLLM 0.22.1 can mis-detect its vendored DeepGEMM as usable on H100 and fail while
# warming a BF16 model (tools/modal-kernel-poc), so the BF16 model runs without it.
QWEN3_8B = Model("Qwen/Qwen3-8B", "b968826d9c46dd6066d109eabc6255188de91218", {"VLLM_USE_DEEP_GEMM": "0"}, (),
                 "vLLM defaults, the current setup of the 2026-11-20 campaign")
# The most downloaded FP8 checkpoint from Qwen, Z.ai and DeepSeek that fits one H100 on
# 2026-11-21 (poc_modal.py choose-model). Its command is vLLM's own recipe for one H100 in FP8,
# the page the model card links; the recipe needs vLLM 0.17 or later and transformers
# 5.8.0 or later, and the pinned image has vLLM 0.22.1 and transformers 5.10.2.
QWEN38_27B_FP8 = Model("Qwen/Qwen3.8-27B-FP8", "c3a462cc3d38cf351f3b26cbd763469eeb74ef81", {},
                       ("--tensor-parallel-size", "1", "--enable-auto-tool-choice", "--tool-call-parser", "qwen3_coder",
                        "--reasoning-parser", "qwen3", "--mm-encoder-tp-mode", "data"),
                       "https://recipes.vllm.ai/Qwen/Qwen3.8-27B.json?hardware=h100&variant=fp8&strategy=single_node_tp (read 2026-11-21)",
                       # 48 of its 64 layers keep a recurrent state, one cache block per running
                       # request. On one H100 (2026-11-21 check) vLLM 0.22.1 had 818 such blocks
                       # and refused to start with its default of 1,024 requests at once. The
                       # campaign runs 32 at once, so 512 changes nothing it measures.
                       # The publisher sets VLLM_USE_RUST_FRONTEND=1. In the pinned image its Rust
                       # HTTP frontend exited with code 1 once the engine was ready (2026-11-21
                       # check), so every setup uses vLLM's Python frontend, which the recipe
                       # also offers for images without the Rust one.
                       publisher_env={"VLLM_USE_RUST_FRONTEND": "1"},
                       added_args=("--max-num-seqs", "512"),
                       added_because=("vLLM 0.22.1 does not start the publisher's command on one H100: max_num_seqs 1024 exceeds the 818 recurrent-state cache blocks, "
                                      "and vLLM asks for at most 818. Its Rust frontend (VLLM_USE_RUST_FRONTEND=1) exited at startup, so the Python frontend serves."),
                       max_num_seqs=818)
MODELS = {model.id: model for model in (QWEN3_8B, QWEN38_27B_FP8)}


# Arguments a pod may pass to vLLM: the publishers' commands and the single-GPU changes the
# planner's diagnosis rules make. Anything else is refused, so a candidate can never ask
# for more GPUs, another port, or another model. Speculative decoding is refused too: the
# replayed traffic has no real text, so a draft's acceptance would not be honest.
VALUE_FLAGS = {"--served-model-name", "--max-model-len", "--quantization", "--kv-cache-dtype", "--max-num-batched-tokens",
               "--max-num-seqs", "--gpu-memory-utilization", "--block-size", "--seed", "--dtype", "--tensor-parallel-size",
               "--tool-call-parser", "--reasoning-parser", "--mm-encoder-tp-mode", "--api-server-count", "--stream-interval",
               "--max-cudagraph-capture-size"}
SWITCH_FLAGS = {"--enable-prefix-caching", "--no-enable-prefix-caching", "--enable-chunked-prefill", "--no-enable-chunked-prefill",
                "--async-scheduling", "--no-async-scheduling", "--calculate-kv-scales", "--enable-auto-tool-choice"}
# Values a flag must keep: one GPU per server, and a bounded number of API processes.
FIXED_VALUES = {"--tensor-parallel-size": {"1"}, "--mm-encoder-tp-mode": {"data", "weights"}, "--api-server-count": {"1", "2", "3", "4"}}


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
