"""Wrong: skips the epsilon term, so near-zero rows are not normalized correctly."""

import torch
import triton
import triton.language as tl


@triton.jit
def _kernel(x_ptr, residual_ptr, weight_ptr, output_ptr, residual_output_ptr, hidden_size: tl.constexpr, epsilon: tl.constexpr, block_size: tl.constexpr):
    row = tl.program_id(axis=0)
    offsets = tl.arange(0, block_size)
    mask = offsets < hidden_size
    row_offsets = row * hidden_size + offsets
    x = tl.load(x_ptr + row_offsets, mask=mask, other=0.0).to(tl.float32)
    residual = tl.load(residual_ptr + row_offsets, mask=mask, other=0.0).to(tl.float32)
    fused = x + residual
    inverse_rms = 1.0 / tl.sqrt(tl.sum(fused * fused, axis=0) / hidden_size + 1e-20)
    weight = tl.load(weight_ptr + offsets, mask=mask, other=0.0).to(tl.float32)
    tl.store(residual_output_ptr + row_offsets, fused, mask=mask)
    tl.store(output_ptr + row_offsets, fused * inverse_rms * weight, mask=mask)


def fused_residual_rmsnorm(x, residual, weight, epsilon):
    x, residual, weight = x.contiguous(), residual.contiguous(), weight.contiguous()
    output, residual_output = torch.empty_like(x), torch.empty_like(x)
    hidden_size = x.shape[-1]
    _kernel[(x.numel() // hidden_size,)](x, residual, weight, output, residual_output, hidden_size=hidden_size, epsilon=epsilon, block_size=triton.next_power_of_2(hidden_size))
    return output, residual_output
