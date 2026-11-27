"""How an EAGLE-3 draft scores on rows whose hidden states are already written, without the
trainer's loop: speculators' own forward (compiled on CUDA unless TORCHDYNAMO_DISABLE=1) and an
independent plain-torch forward written from vLLM 0.34's llama_eagle3 math, on the same rows.

It also checks the data: the verifier's own prediction from the final hidden state must be the
next answer token (the answers are the model's greedy output), so a low share there means the
hidden states are not the model's.

    TORCHDYNAMO_DISABLE=1 python trainer_view.py --draft DIR --verifier DIR --data DIR --rows 128 --out view.json
"""

from __future__ import annotations

import argparse
import json
import math
import os
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--draft", required=True, help="a speculators-format EAGLE-3 draft")
    parser.add_argument("--verifier", required=True)
    parser.add_argument("--data", required=True, type=Path, help="the trainer's data directory: the dataset and hidden_states/hs_{i}.safetensors")
    parser.add_argument("--rows", type=int, default=128, help="score the last N rows (the held-out tail)")
    parser.add_argument("--pack-len", type=int, default=4096, help="the trainer's --total-seq-len")
    parser.add_argument("--out", required=True, type=Path)
    options = parser.parse_args()

    import torch
    from datasets import load_from_disk
    from safetensors.torch import load_file
    from speculators.models.eagle3.config import Eagle3SpeculatorConfig
    from speculators.models.eagle3.core import Eagle3DraftModel

    device = "cuda" if torch.cuda.is_available() else "cpu"
    config = Eagle3SpeculatorConfig.from_pretrained(options.draft)
    config.transformer_layer_config._attn_implementation = "sdpa"  # noqa: SLF001
    model = Eagle3DraftModel.from_pretrained(options.draft, config=config, t2d=None, d2t=None, verifier=options.verifier).to(device, dtype=torch.bfloat16).eval()
    layer, attention = model.layers[0], model.layers[0].self_attn
    tl = config.transformer_layer_config
    heads, kv_heads, head_dim = tl.num_attention_heads, tl.num_key_value_heads, tl.head_dim
    theta = float((getattr(tl, "rope_parameters", None) or {}).get("rope_theta") or getattr(tl, "rope_theta", 10000.0))
    # The data check reads the verifier's own embedding (its tied head), never the draft's copy.
    from safetensors import safe_open

    from draft_embedding import VERIFIER_EMBEDDING, verifier_file

    with safe_open(str(verifier_file(options.verifier)), framework="pt") as handle:
        full_head = handle.get_tensor(VERIFIER_EMBEDDING).to(device)
    draft_embedding_is_verifiers = bool(torch.equal(model.embed_tokens.weight.detach().to(full_head.dtype), full_head))

    def rms(x, weight, eps=tl.rms_norm_eps):
        x = x.float()
        return x * torch.rsqrt(x.pow(2).mean(-1, keepdim=True) + eps) * weight.float()

    def rope(x, positions):
        half = x.shape[-1] // 2
        inv = 1.0 / (theta ** (torch.arange(0, half, device=x.device, dtype=torch.float32) / half))
        freqs = positions.float()[:, None] * inv[None, :]
        cos, sin = torch.cat([freqs.cos()] * 2, -1), torch.cat([freqs.sin()] * 2, -1)
        return x * cos + torch.cat([-x[..., half:], x[..., :half]], -1) * sin

    def linear(module, x):
        return x @ module.weight.float().T

    def reference(aux, input_ids, positions):
        """vLLM 0.34 llama_eagle3: fc, then layer 0 with the embedding normed by
        input_layernorm and the hidden state by hidden_norm, residual from the un-normed hidden
        state, causal attention with RoPE, the MLP, the final norm, the draft head."""
        seq = input_ids.shape[0]
        hidden = linear(model.fc, aux.float())
        residual = hidden
        x = torch.cat([rms(model.embed_tokens(input_ids), layer.input_layernorm.weight), rms(hidden, layer.hidden_norm.weight)], -1)
        q = linear(attention.q_proj, x).view(seq, heads, head_dim).transpose(0, 1)
        k = linear(attention.k_proj, x).view(seq, kv_heads, head_dim).transpose(0, 1)
        v = linear(attention.v_proj, x).view(seq, kv_heads, head_dim).transpose(0, 1)
        q, k = rope(q, positions), rope(k, positions)
        k, v = k.repeat_interleave(heads // kv_heads, 0), v.repeat_interleave(heads // kv_heads, 0)
        scores = (q @ k.transpose(-1, -2)) / math.sqrt(head_dim)
        scores = scores.masked_fill(torch.triu(torch.ones(seq, seq, dtype=torch.bool, device=scores.device), 1), float("-inf"))
        out = (scores.softmax(-1) @ v).transpose(0, 1).reshape(seq, heads * head_dim)
        hidden = linear(attention.o_proj, out) + residual
        mlp = layer.mlp
        normed = rms(hidden, layer.post_attention_layernorm.weight)
        hidden = linear(mlp.down_proj, torch.nn.functional.silu(linear(mlp.gate_proj, normed)) * linear(mlp.up_proj, normed)) + hidden
        return linear(model.lm_head, rms(hidden, model.norm.weight))

    dataset = load_from_disk(str(options.data))
    total = len(dataset)
    counts = {"positions": 0, "speculators_correct": 0, "reference_correct": 0, "forward_agree": 0, "verifier_predicts_next": 0, "verifier_positions": 0, "target_in_draft_vocab": 0,
              "packs": 0}

    def flush(pack: list[dict]) -> None:
        """speculators' forward on rows packed as the trainer's collate packs them: one
        sequence of options.pack_len tokens, document ids per row and -1 for padding, positions
        per row and 0 for padding, so the compiled graph keeps one shape."""
        if not pack:
            return
        size, used = options.pack_len, sum(row["input_ids"].shape[0] for row in pack)
        input_ids = torch.zeros(size, dtype=torch.long, device=device)
        aux = torch.zeros(size, pack[0]["aux"].shape[1], dtype=torch.bfloat16, device=device)
        documents = torch.full((size,), -1, dtype=torch.long, device=device)
        positions = torch.zeros(size, dtype=torch.long, device=device)
        offset = 0
        for number, row in enumerate(pack):
            n = row["input_ids"].shape[0]
            input_ids[offset:offset + n], aux[offset:offset + n], positions[offset:offset + n] = row["input_ids"], row["aux"], row["positions"]
            documents[offset:offset + n] = number
            offset += n
        with torch.no_grad():
            tokens = model(hidden_states=aux[None], input_ids=input_ids[None], document_ids=documents[None], position_ids=positions[None], ttt_steps=1)[0][0][:used]
        counts["packs"] += 1
        offset = 0
        for row in pack:
            n = row["input_ids"].shape[0]
            mine, loss_mask = row["reference"], row["loss_mask"]
            theirs = tokens[offset:offset + n]
            offset += n
            counts["positions"] += int(loss_mask.sum())
            counts["speculators_correct"] += int((theirs == row["targets"])[loss_mask].sum())
            counts["reference_correct"] += int((mine == row["targets"])[loss_mask].sum())
            counts["forward_agree"] += int((mine == theirs)[loss_mask].sum())

    pack: list[dict] = []
    for index in range(max(0, total - options.rows), total):
        saved = load_file(str(options.data / "hidden_states" / f"hs_{index}.safetensors"))
        ids = saved["token_ids"].to(device)
        if not torch.equal(ids.cpu(), torch.as_tensor(dataset[index]["input_ids"]).long()):
            raise SystemExit(f"hs_{index} does not belong to row {index}")
        hidden = saved["hidden_states"].to(device, torch.bfloat16)
        mask = torch.as_tensor(dataset[index]["loss_mask"]).bool().to(device)
        # speculators' shift: token x[j+1] with hidden g[j] predicts the verifier's choice from y[j+1].
        input_ids, aux, last, loss_mask = ids[1:], hidden[:-1, :3].flatten(1), hidden[1:, 3], mask[1:]
        positions = torch.arange(1, ids.shape[0], device=device)
        if input_ids.shape[0] > options.pack_len:
            continue
        with torch.no_grad():
            targets = model.verifier_lm_head(model.verifier_norm(last)).argmax(-1)
            mine = reference(aux, input_ids, positions).argmax(-1)
            # The verifier's full-vocabulary choice from y[j+1] is the token after x[j+1].
            full = (model.verifier_norm(last).float() @ full_head.float().T).argmax(-1)
        selected = mask[2:]  # x[j+2] is an answer token
        counts["verifier_predicts_next"] += int((full[:-1] == ids[2:])[selected].sum())
        counts["verifier_positions"] += int(selected.sum())
        if model.t2d is not None:
            counts["target_in_draft_vocab"] += int(model.t2d[ids[2:]][selected].sum())
        if sum(row["input_ids"].shape[0] for row in pack) + input_ids.shape[0] > options.pack_len:
            flush(pack)
            pack = []
        pack.append({"input_ids": input_ids, "aux": aux, "positions": positions, "loss_mask": loss_mask, "targets": targets, "reference": mine})
    flush(pack)
    positions = max(1, counts["positions"])
    out = {"compiled": os.environ.get("TORCHDYNAMO_DISABLE") != "1" and device == "cuda", "device": device, "rows": min(options.rows, total),
           "draft_embedding_is_verifiers": draft_embedding_is_verifiers, **counts,
           "speculators_first_token_accuracy": round(counts["speculators_correct"] / positions, 4),
           "reference_first_token_accuracy": round(counts["reference_correct"] / positions, 4),
           "forwards_agree": round(counts["forward_agree"] / positions, 4),
           "verifier_predicts_next_token": round(counts["verifier_predicts_next"] / max(1, counts["verifier_positions"]), 4),
           "next_token_in_draft_vocab": round(counts["target_in_draft_vocab"] / max(1, counts["verifier_positions"]), 4)}
    options.out.write_text(json.dumps(out, indent=1))
    print(json.dumps(out))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
