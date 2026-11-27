"""Rung 1 of the trained-draft ladder: acceptance offline, $0, on this machine.

    python3 rung1_offline.py --run-dir .infercrane/trained-draft/rung1 --speculators /path/bin/speculators

Rules: rules-rung1-offline-qwen3-1.7b-2026-11-26.md. Needs torch, transformers, safetensors and
speculators 0.9.0 (one virtual environment). Steps, each skipped when its output exists:

  answers   the model's greedy answers to 512 training and 128 held-out oasst1 prompts
  hidden    hidden states of every row in speculators' file format (hs_{i}.safetensors)
  prepare   speculators prepare-data on the token rows (training rows first, held out last)
  convert   the published EAGLE-3 draft into speculators' format
  published speculators' validation of the unchanged draft (learning rate 0) on the held-out rows
  trained   the draft fine-tuned on the training rows, validated on the same held-out rows
  simulate  n-gram and suffix drafting replayed exactly against the held-out greedy answers
  report    rung1.json with every arm's tokens a step and the pass bar
"""

from __future__ import annotations

import argparse
import json
import os
import random
import re
import subprocess
import sys
import time
from collections import Counter, defaultdict
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
import draftlib as lib  # noqa: E402

MODEL, MODEL_REVISION = "Qwen/Qwen3-1.7B", "70d244cc86ccca08cf5af4e1e306ecf908b1ad5e"
DRAFT, DRAFT_REVISION = "AngelSlim/Qwen3-1.7B_eagle3", "94441b48acc5804677ae12259617c83323b543a9"
HERE = Path(__file__).resolve().parent
LAYERS = [2, 14, 25]  # speculators' default for 28 layers: 2, n/2, n-3; plus the final state
TRAIN_PROMPTS, HELD_OUT_PROMPTS, MAX_NEW_TOKENS = 512, 128, 320
DRAFT_TOKENS = 3


def log(message: str) -> None:
    print(time.strftime("%H:%M:%S"), message, flush=True)


# ---------------------------------------------------------------------------
# Answers and hidden states (transformers on the local GPU or CPU).
# ---------------------------------------------------------------------------


def device():
    import torch

    return "mps" if torch.backends.mps.is_available() else "cuda" if torch.cuda.is_available() else "cpu"


def load_model():
    import torch
    from huggingface_hub import snapshot_download
    from transformers import AutoModelForCausalLM, AutoTokenizer

    path = snapshot_download(MODEL, revision=MODEL_REVISION)
    tokenizer = AutoTokenizer.from_pretrained(path)
    model = AutoModelForCausalLM.from_pretrained(path, dtype=torch.bfloat16).to(device()).eval()
    return path, tokenizer, model


def prompt_ids(tokenizer, content: str) -> list[int]:
    text = tokenizer.apply_chat_template([{"role": "user", "content": content}], tokenize=False, add_generation_prompt=True, enable_thinking=False)
    return tokenizer(text, add_special_tokens=False)["input_ids"]


def answers(tokenizer, model, prompts: list[dict], batch: int = 16) -> list[dict]:
    """Greedy answers, batched with left padding; each answer stops at the end of turn."""
    import torch

    end = {tokenizer.convert_tokens_to_ids("<|im_end|>"), tokenizer.eos_token_id}
    out = []
    for start in range(0, len(prompts), batch):
        group = prompts[start:start + batch]
        ids = [prompt_ids(tokenizer, p["content"]) for p in group]
        width = max(len(x) for x in ids)
        pad = tokenizer.pad_token_id if tokenizer.pad_token_id is not None else tokenizer.eos_token_id
        input_ids = torch.tensor([[pad] * (width - len(x)) + x for x in ids], device=model.device)
        mask = torch.tensor([[0] * (width - len(x)) + [1] * len(x) for x in ids], device=model.device)
        with torch.no_grad():
            generated = model.generate(input_ids=input_ids, attention_mask=mask, max_new_tokens=MAX_NEW_TOKENS, do_sample=False, pad_token_id=pad)
        for prompt, row_ids, row in zip(group, ids, generated.tolist()):
            answer = row[width:]
            cut = next((i + 1 for i, token in enumerate(answer) if token in end), len(answer))
            out.append({"id": prompt["id"], "lang": prompt.get("lang", ""), "prompt_ids": row_ids, "answer_ids": answer[:cut]})
        log(f"answers {len(out)}/{len(prompts)}")
    return out


def hidden_states(model, rows: list[dict], directory: Path) -> None:
    """vLLM's convention: layer i is the input to decoder layer i (HF hidden_states[i]); the
    last entry is the final layer's output before the norm (a hook, since HF's last entry is
    normalised)."""
    import torch
    from safetensors.torch import save_file

    directory.mkdir(parents=True, exist_ok=True)
    final = {}
    hook = model.model.layers[-1].register_forward_hook(lambda _m, _i, output: final.__setitem__("h", output[0] if isinstance(output, tuple) else output))
    try:
        for index, row in enumerate(rows):
            target = directory / f"hs_{index}.safetensors"
            if target.exists():
                continue
            tokens = torch.tensor([row["input_ids"]], device=model.device)
            with torch.no_grad():
                result = model(input_ids=tokens, output_hidden_states=True)
            layers = [result.hidden_states[i][0] for i in LAYERS] + [final["h"][0]]
            save_file({"hidden_states": torch.stack(layers, dim=1).to(torch.bfloat16).cpu().contiguous(), "token_ids": tokens[0].cpu().to(torch.long)}, str(target))
            if index % 64 == 0:
                log(f"hidden states {index}/{len(rows)}")
    finally:
        hook.remove()


# ---------------------------------------------------------------------------
# speculators.
# ---------------------------------------------------------------------------


def reorder(source: Path, target: Path, train_ids: list[list[int]], held_out_ids: list[list[int]]) -> None:
    """The prepared dataset in this run's order: training rows (in prepared order), then
    held-out rows, so the trainer's validation tail is exactly the held-out set. Refuses a
    dataset that lost or gained a row."""
    from datasets import load_from_disk

    dataset = load_from_disk(str(source))
    held = {tuple(ids) for ids in held_out_ids}
    train = {tuple(ids) for ids in train_ids}
    train_rows, held_rows = [], []
    for index, ids in enumerate(dataset["input_ids"]):
        key = tuple(int(x) for x in ids)
        if key in held:
            held_rows.append(index)
        elif key in train:
            train_rows.append(index)
    if len(train_rows) != len(train_ids) or len(held_rows) != len(held_out_ids):
        raise SystemExit(f"prepare-data kept {len(train_rows)} of {len(train_ids)} training and {len(held_rows)} of {len(held_out_ids)} held-out rows")
    dataset.select(train_rows + held_rows).save_to_disk(str(target))


def run(command: list[str], log_path: Path) -> int:
    log("run " + " ".join(command[:4]) + " ...")
    # A wide terminal keeps rich from wrapping one log record across lines.
    env = {**os.environ, "COLUMNS": "100000"}
    with log_path.open("w") as handle:
        return subprocess.run(command, stdout=handle, stderr=subprocess.STDOUT, check=False, env=env).returncode  # noqa: S603


def saved_val_metrics(save_path: Path) -> list[dict]:
    """The validation passes the trainer saved (checkpoints/<epoch>/val_metrics.json), in
    epoch order; an epoch whose loss did not improve writes none."""
    passes = []
    for path in sorted(save_path.glob("*/val_metrics.json"), key=lambda p: int(p.parent.name) if p.parent.name.isdigit() else -1):
        metrics = json.loads(path.read_text())
        found: dict = {"cond_acc": {}, "full_acc": {}}
        for key, value in metrics.items():
            match = re.fullmatch(r"(cond_acc|full_acc)_(\d+)_epoch", key)
            if match:
                found[match.group(1)][int(match.group(2))] = float(value)
        if found["cond_acc"]:
            passes.append({"epoch": path.parent.name, **{name: [values[k] for k in sorted(values)] for name, values in found.items()}})
    return passes


_VAL = re.compile(r"'?(cond_acc|full_acc)_(\d+)_epoch'?\s*[:=]\s*([0-9.eE+-]+)")


def val_metrics(log_path: Path) -> list[dict]:
    """Every validation pass in a trainer log: {cond_acc: [a0, a1, a2], full_acc: [...]}."""
    passes = []
    for line in log_path.read_text(errors="replace").splitlines():
        found = _VAL.findall(line)
        if not found:
            continue
        metrics: dict = {"cond_acc": {}, "full_acc": {}}
        for name, step, value in found:
            metrics[name][int(step)] = float(value)
        passes.append({name: [values[k] for k in sorted(values)] for name, values in metrics.items()})
    return passes


def tokens_per_step(acceptance: list[float], draft_tokens: int) -> float:
    expected, survive = 1.0, 1.0
    for share in acceptance[:draft_tokens]:
        survive *= share
        expected += survive
    return expected


# ---------------------------------------------------------------------------
# Model-free drafting, replayed against the greedy answers.
# ---------------------------------------------------------------------------


def ngram_proposal(tokens: list[int], k: int, lo: int = 2, hi: int = 5) -> list[int]:
    """vLLM's prompt lookup: the longest n-gram (hi down to lo) ending the sequence that also
    occurs earlier proposes the k tokens after its latest earlier occurrence."""
    for n in range(min(hi, len(tokens) - 1), lo - 1, -1):
        tail = tokens[-n:]
        for start in range(len(tokens) - n - 1, -1, -1):
            if tokens[start:start + n] == tail:
                return tokens[start + n:start + n + k]
    return []


class SuffixIndex:
    """Continuation counts for every context up to `depth` tokens: the same lookups a suffix
    tree answers (longest matching suffix, most frequent next token)."""

    def __init__(self, depth: int = 24):
        self.depth, self.counts = depth, defaultdict(Counter)

    def add(self, tokens: list[int], start: int = 0) -> None:
        for end in range(max(1, start), len(tokens)):
            for n in range(1, min(self.depth, end) + 1):
                self.counts[tuple(tokens[end - n:end])][tokens[end]] += 1


def suffix_proposal(context: list[int], indexes: list[SuffixIndex], k: int, min_prob: float = 0.1) -> list[int]:
    """Longest suffix of the context any index has seen; then follow the most frequent next
    token while its share stays at least min_prob, up to min(k, matched length) tokens."""
    def lookup(key):
        merged = Counter()
        for index in indexes:
            merged.update(index.counts.get(key, {}))
        return merged

    match = 0
    for n in range(min(24, len(context)), 0, -1):
        if lookup(tuple(context[-n:])):
            match = n
            break
    if not match:
        return []
    proposal, window = [], list(context[-match:])
    while len(proposal) < min(k, match):
        counts = lookup(tuple(window))
        if not counts:
            break
        token, count = counts.most_common(1)[0]
        if count / sum(counts.values()) < min_prob:
            break
        proposal.append(token)
        window = (window + [token])[-24:]
    return proposal


def replay(rows: list[dict], propose) -> dict:
    """Greedy verification against the model's own answer: a step accepts the drafted prefix
    that equals the answer and writes one more token."""
    steps = tokens = 0
    for row in rows:
        context, answer, i = list(row["prompt_ids"]), row["answer_ids"], 0
        while i < len(answer):
            draft = propose(row, context)
            accepted = 0
            while accepted < len(draft) and i + accepted < len(answer) and draft[accepted] == answer[i + accepted]:
                accepted += 1
            advance = min(accepted + 1, len(answer) - i)
            context.extend(answer[i:i + advance])
            i += advance
            steps += 1
            tokens += advance
    return {"steps": steps, "tokens": tokens, "tokens_per_step": round(tokens / steps, 4) if steps else None}


def simulate(held_out: list[dict]) -> dict:
    out = {"ngram_k3": replay(held_out, lambda _row, context: ngram_proposal(context, 3))}
    for k in (3, 8):
        corpus = SuffixIndex()
        steps = tokens = 0
        for row in held_out:
            request = SuffixIndex()
            request.add(row["prompt_ids"])
            seen = len(row["prompt_ids"])

            def propose(_row, context, request=request):
                nonlocal seen
                request.add(context, start=seen)
                seen = len(context)
                return suffix_proposal(context, [request, corpus], k)

            result = replay([row], propose)
            steps, tokens = steps + result["steps"], tokens + result["tokens"]
            corpus.add(row["answer_ids"])  # later requests see earlier answers, as vLLM's corpus does
        out[f"suffix_k{k}"] = {"steps": steps, "tokens": tokens, "tokens_per_step": round(tokens / steps, 4), "approximation": True}
    return out


# ---------------------------------------------------------------------------
# The run.
# ---------------------------------------------------------------------------


def pass_bar(published: dict, trained: dict, best_free: float) -> dict:
    return {
        "conversion_sanity": "unknown" if not published else ("pass" if published["tokens_per_step_k2"] >= 1.9 else "fail"),
        "drafts_are_the_lever": "unknown" if not published else ("pass" if published["tokens_per_step_k3"] - best_free >= 0.2 else "fail"),
        "fine_tuning_moves_the_right_way": "unknown" if not (published and trained) else ("pass" if trained["tokens_per_step_k3"] >= published["tokens_per_step_k3"] - 0.02 else "fail"),
        "best_model_free_tokens_per_step": best_free,
    }


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--run-dir", required=True, type=Path)
    parser.add_argument("--prompts", type=Path, default=Path(".infercrane/trained-draft/td-1/prompts"))
    parser.add_argument("--speculators", required=True)
    parser.add_argument("--train-epochs", type=int, default=2)
    options = parser.parse_args()
    run_dir = options.run_dir
    run_dir.mkdir(parents=True, exist_ok=True)
    started = time.time()

    rows_path = run_dir / "answers.json"
    if not rows_path.exists():
        rng = random.Random(lib.SEED)
        train = json.loads((options.prompts / "train.json").read_text())
        held_out = json.loads((options.prompts / "held_out.json").read_text())[:HELD_OUT_PROMPTS]
        train = rng.sample(train, TRAIN_PROMPTS)
        _, tokenizer, model = load_model()
        rows = {"train": answers(tokenizer, model, train), "held_out": answers(tokenizer, model, held_out)}
        lib.write_json(rows_path, rows)
        del model
    rows = json.loads(rows_path.read_text())
    ordered = rows["train"] + rows["held_out"]
    for row in ordered:
        row["input_ids"] = row["prompt_ids"] + row["answer_ids"]

    # prepare-data shuffles rows (seed 0). The trainer splits its dataset into a contiguous train
    # head and validation tail and reads hidden states as hs_{row}, so the prepared rows are put
    # back in this run's order (training rows, then held-out rows) before any hidden state is
    # written, and every file is written from the row it belongs to.
    model_path = subprocess.run([sys.executable, "-c", f"from huggingface_hub import snapshot_download as s; print(s('{MODEL}', revision='{MODEL_REVISION}'))"], capture_output=True, text=True, check=True).stdout.strip()  # noqa: S603
    shuffled, data = run_dir / "data-prepared", run_dir / "data"
    if not (shuffled / "dataset_info.json").exists():
        jsonl = run_dir / "rows.jsonl"
        with jsonl.open("w") as handle:
            for row in ordered:
                handle.write(json.dumps({"input_ids": row["input_ids"], "loss_mask": [0] * len(row["prompt_ids"]) + [1] * len(row["answer_ids"])}) + "\n")
        code = run([options.speculators, "prepare-data", "--model", model_path, "--data", str(jsonl), "--output", str(shuffled), "--seq-length", "4096", "--minimum-valid-tokens", "1"], run_dir / "prepare-data.log")
        if code:
            raise SystemExit(f"prepare-data exited {code}")
    if not (data / "dataset_info.json").exists():
        reorder(shuffled, data, [row["input_ids"] for row in rows["train"]], [row["input_ids"] for row in rows["held_out"]])
    if not (data / "hidden_states" / f"hs_{len(ordered) - 1}.safetensors").exists():
        from datasets import load_from_disk

        prepared = [{"input_ids": [int(x) for x in item]} for item in load_from_disk(str(data))["input_ids"]]
        _, _, model = load_model()
        hidden_states(model, prepared, data / "hidden_states")
        del model
    draft_path = subprocess.run([sys.executable, "-c", f"from huggingface_hub import snapshot_download as s; print(s('{DRAFT}', revision='{DRAFT_REVISION}'))"], capture_output=True, text=True, check=True).stdout.strip()  # noqa: S603

    converted = run_dir / "published-speculators"
    if not (converted / "config.json").exists():
        code = run([options.speculators, "convert", draft_path, "--algorithm", "eagle3", "--verifier", model_path, "--output-path", str(converted)], run_dir / "convert.log")
        if code:
            raise SystemExit(f"convert exited {code}")
    # speculators 0.9.0 would train and validate the converted draft with zero token embeddings
    # (draft_embedding.py); the verifier's embedding goes into the checkpoint first.
    from draft_embedding import add_verifier_embedding

    embedding = add_verifier_embedding(converted, model_path)

    ratio = len(rows["train"]) / len(ordered)
    common = ["train", "--speculator-type", "eagle3", "--from-pretrained", str(converted), "--verifier-name-or-path", model_path, "--data-path", str(data),
              "--on-missing", "raise", "--train-data-ratio", f"{ratio:.6f}", "--total-seq-len", "4096", "--draft-attn-impl", "sdpa", "--loss-implementation", "eager", "--num-workers", "0",
              "--seed", str(lib.SEED), "--log-freq", "5", "--checkpoint-freq", "1"]
    results: dict = {"model": MODEL, "model_revision": MODEL_REVISION, "draft": DRAFT, "draft_revision": DRAFT_REVISION, "device": device(),
                     "train_rows": len(rows["train"]), "held_out_rows": len(rows["held_out"]),
                     "held_out_answer_tokens": sum(len(r["answer_ids"]) for r in rows["held_out"]), "train_answer_tokens": sum(len(r["answer_ids"]) for r in rows["train"]),
                     "embedding": embedding}
    # The draft scored on the held-out rows without the trainer's loop: speculators' forward
    # against an independent forward written from vLLM's math, whether the draft's embedding is
    # the verifier's, and the data's own check (the verifier's choice from the final hidden state
    # is the next answer token).
    view = run_dir / "view.json"
    if not view.exists():
        results["view_exit"] = run([sys.executable, str(HERE / "trainer_view.py"), "--draft", str(converted), "--verifier", model_path, "--data", str(data),
                                    "--rows", str(len(rows["held_out"])), "--out", str(view)], run_dir / "view.log")
    results["view"] = json.loads(view.read_text()) if view.exists() else None
    learn = ["--lr", "1e-4", "--epochs", str(options.train_epochs), "--scheduler-type", "cosine"]
    frozen = ["--lr", "0", "--epochs", "1", "--max-steps", "1"]
    for arm, extra in (("published", frozen), ("trained", learn)):
        log_path, save_path = run_dir / f"{arm}.log", run_dir / f"{arm}-checkpoints"
        if not saved_val_metrics(save_path) and not (log_path.exists() and val_metrics(log_path)):
            code = run([options.speculators, *common, *extra, "--save-path", str(save_path), "--no-resume-from-checkpoint"], log_path)
            results[f"{arm}_exit"] = code
        passes = saved_val_metrics(save_path) or val_metrics(log_path)
        if passes:
            cond = passes[-1]["cond_acc"]
            results[arm] = {"cond_acc": cond, "full_acc": passes[-1]["full_acc"], "tokens_per_step_k2": round(tokens_per_step(cond, 2), 4), "tokens_per_step_k3": round(tokens_per_step(cond, 3), 4),
                            "validation_passes": len(passes), "epoch": passes[-1].get("epoch", "log")}

    results["model_free"] = simulate(rows["held_out"])
    published = results.get("published", {})
    trained = results.get("trained", {})
    best_free = max(v["tokens_per_step"] for v in results["model_free"].values() if v["tokens_per_step"])
    results["pass_bar"] = pass_bar(published, trained, best_free)
    results["wall_seconds"] = round(time.time() - started, 1)
    lib.write_json(run_dir / "rung1.json", results)
    print(json.dumps({k: results[k] for k in ("embedding", "view", "published", "trained", "model_free", "pass_bar") if k in results}, indent=1))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
