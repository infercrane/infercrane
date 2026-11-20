"""Wrong on purpose: applies SiLU to the up projection instead of the gate."""

import torch
import triton
import triton.language as tl


@triton.jit
def _gate_up(x_ptr, w13_ptr, ids_ptr, act_ptr, hidden, intermediate, top_k: tl.constexpr, block_i: tl.constexpr, block_k: tl.constexpr):
    pair = tl.program_id(axis=0)
    tile = tl.program_id(axis=1)
    token = pair // top_k
    expert = tl.load(ids_ptr + pair).to(tl.int64)
    rows = tile * block_i + tl.arange(0, block_i)
    row_mask = rows < intermediate
    gate = tl.zeros((block_i,), dtype=tl.float32)
    up = tl.zeros((block_i,), dtype=tl.float32)
    for start in range(0, hidden, block_k):
        cols = start + tl.arange(0, block_k)
        col_mask = cols < hidden
        x = tl.load(x_ptr + token * hidden + cols, mask=col_mask, other=0.0).to(tl.float32)
        base = w13_ptr + expert * 2 * intermediate * hidden
        mask = row_mask[:, None] & col_mask[None, :]
        w_gate = tl.load(base + rows[:, None] * hidden + cols[None, :], mask=mask, other=0.0).to(tl.float32)
        w_up = tl.load(base + (rows[:, None] + intermediate) * hidden + cols[None, :], mask=mask, other=0.0).to(tl.float32)
        gate += tl.sum(w_gate * x[None, :], axis=1)
        up += tl.sum(w_up * x[None, :], axis=1)
    activated = up / (1.0 + tl.exp(-up)) * gate
    tl.store(act_ptr + pair * intermediate + rows, activated, mask=row_mask)


@triton.jit
def _down(act_ptr, w2_ptr, ids_ptr, weights_ptr, out_ptr, hidden, intermediate, top_k: tl.constexpr, block_h: tl.constexpr, block_i: tl.constexpr):
    token = tl.program_id(axis=0)
    tile = tl.program_id(axis=1)
    rows = tile * block_h + tl.arange(0, block_h)
    row_mask = rows < hidden
    total = tl.zeros((block_h,), dtype=tl.float32)
    for slot in range(top_k):
        pair = token * top_k + slot
        expert = tl.load(ids_ptr + pair).to(tl.int64)
        weight = tl.load(weights_ptr + pair)
        partial = tl.zeros((block_h,), dtype=tl.float32)
        for start in range(0, intermediate, block_i):
            cols = start + tl.arange(0, block_i)
            col_mask = cols < intermediate
            act = tl.load(act_ptr + pair * intermediate + cols, mask=col_mask, other=0.0)
            w = tl.load(w2_ptr + expert * hidden * intermediate + rows[:, None] * intermediate + cols[None, :], mask=row_mask[:, None] & col_mask[None, :], other=0.0).to(tl.float32)
            partial += tl.sum(w * act[None, :], axis=1)
        total += weight * partial
    tl.store(out_ptr + token * hidden + rows, total.to(tl.bfloat16), mask=row_mask)


def fused_moe_experts(hidden_states, w13, w2, topk_weights, topk_ids):
    tokens, hidden = hidden_states.shape
    intermediate = w2.shape[2]
    top_k = topk_ids.shape[1]
    ids = topk_ids.contiguous().reshape(-1)
    weights = topk_weights.contiguous().reshape(-1)
    act = torch.empty((tokens * top_k, intermediate), device=hidden_states.device, dtype=torch.float32)
    out = torch.empty((tokens, hidden), device=hidden_states.device, dtype=torch.bfloat16)
    _gate_up[(tokens * top_k, triton.cdiv(intermediate, 32))](hidden_states, w13.contiguous(), ids, act, hidden, intermediate, top_k=top_k, block_i=32, block_k=64)
    _down[(tokens, triton.cdiv(hidden, 32))](act, w2.contiguous(), ids, weights, out, hidden, intermediate, top_k=top_k, block_h=32, block_i=64)
    return out
