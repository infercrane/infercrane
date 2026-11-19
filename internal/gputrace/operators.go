package gputrace

import (
	"fmt"
	"strings"

	"github.com/infercrane/infercrane/internal/gpuprofile"
)

// Kernel name fragments for the mixture-of-experts operator classes. Routing
// comes first: its kernels are small and would otherwise read as sampling.
var (
	moeRoutingFragments = []string{"topkgatingsoftmax", "topk_softmax", "moe_align", "grouped_topk", "moe_sum", "count_and_sort_expert"}
	moeExpertFragments  = []string{"fused_moe", "moe_mm", "grouped_gemm", "ck_moe", "fmoe", "moe_wna16"}
)

// minOperatorCoverage is the share of an operator's kernel time that needs a
// host operator with recorded shapes before the analysis states its
// efficiency, as for the roofline.
const minOperatorCoverage = 0.5

// OperatorClass names the operator class of a GPU kernel by its name, or ""
// for a kernel outside the measured classes.
func OperatorClass(name string) string {
	lower := strings.ToLower(name)
	switch {
	case containsAny(lower, communicationFragments):
		return ""
	case containsAny(lower, moeRoutingFragments):
		return gpuprofile.OperatorMoERouting
	case containsAny(lower, moeExpertFragments):
		return gpuprofile.OperatorMoEExperts
	}
	switch Category(name) {
	case gpuprofile.CategoryAttention:
		return gpuprofile.OperatorAttention
	case gpuprofile.CategoryNorm:
		return gpuprofile.OperatorNorm
	}
	return ""
}

// hostOperators are the registered PyTorch, vLLM and SGLang operator names of
// each class. Shapes are read only from these.
var hostOperators = map[string]string{
	"_C::rms_norm": gpuprofile.OperatorNorm, "_C::fused_add_rms_norm": gpuprofile.OperatorNorm,
	"aten::rms_norm": gpuprofile.OperatorNorm, "aten::layer_norm": gpuprofile.OperatorNorm, "aten::native_layer_norm": gpuprofile.OperatorNorm,
	"aten::scaled_dot_product_attention": gpuprofile.OperatorAttention, "aten::_scaled_dot_product_flash_attention": gpuprofile.OperatorAttention,
	"aten::_scaled_dot_product_efficient_attention": gpuprofile.OperatorAttention, "aten::_scaled_dot_product_cudnn_attention": gpuprofile.OperatorAttention,
	"vllm::unified_attention": gpuprofile.OperatorAttention, "vllm::unified_attention_with_output": gpuprofile.OperatorAttention,
	"vllm::outplace_fused_experts": gpuprofile.OperatorMoEExperts, "vllm::inplace_fused_experts": gpuprofile.OperatorMoEExperts,
	"vllm::moe_forward":    gpuprofile.OperatorMoEExperts,
	"_moe_C::topk_softmax": gpuprofile.OperatorMoERouting,
}

// operatorIndex is the 1-based index of a class in gpuprofile.Operators, so
// an Op stores it in one byte; 0 is no class.
func operatorIndex(class string) int8 {
	for index, name := range gpuprofile.Operators {
		if name == class {
			return int8(index + 1)
		}
	}
	return 0
}

// operatorShape computes FLOPs and the minimum bytes moved by a host operator
// of a measured class from its recorded input shapes. Bytes count each
// operand read once and each result written once, and a mixture-of-experts
// operator reads every routed expert's weights as if no two tokens shared an
// expert, so achieved throughput is never understated by the bytes. Attention
// counts every query-key pair, an upper bound for causal attention. An
// operator whose shapes do not carry its cost, such as vLLM's paged attention
// with context lengths in metadata, reports false.
func operatorShape(name string, dims [][]int64, types []string) (float64, float64, bool) {
	size := func(index int) float64 {
		if index < len(types) {
			return dtypeBytes(types[index])
		}
		return 0
	}
	dim := func(index, rank int) []int64 {
		if index < len(dims) && len(dims[index]) == rank {
			return dims[index]
		}
		return nil
	}
	positiveAll := func(values ...int64) bool {
		for _, value := range values {
			if value < 1 {
				return false
			}
		}
		return true
	}
	switch name {
	case "_C::rms_norm":
		input, weight := dim(1, 2), dim(2, 1)
		b := size(1)
		if input == nil || weight == nil || b == 0 || !positiveAll(input[0], input[1]) || weight[0] != input[1] {
			return 0, 0, false
		}
		t, h := float64(input[0]), float64(input[1])
		return 4 * t * h, (2*t*h + h) * b, true
	case "_C::fused_add_rms_norm":
		input, residual, weight := dim(0, 2), dim(1, 2), dim(2, 1)
		b := size(0)
		if input == nil || residual == nil || weight == nil || b == 0 || !positiveAll(input[0], input[1]) || residual[0] != input[0] || residual[1] != input[1] || weight[0] != input[1] {
			return 0, 0, false
		}
		t, h := float64(input[0]), float64(input[1])
		return 5 * t * h, (4*t*h + h) * b, true
	case "aten::rms_norm", "aten::layer_norm", "aten::native_layer_norm":
		if len(dims) == 0 || len(dims[0]) < 2 || size(0) == 0 {
			return 0, 0, false
		}
		input := dims[0]
		h := input[len(input)-1]
		rows := int64(1)
		for _, value := range input[:len(input)-1] {
			rows *= value
		}
		if !positiveAll(rows, h) {
			return 0, 0, false
		}
		t, width, b := float64(rows), float64(h), size(0)
		if name == "aten::rms_norm" {
			return 4 * t * width, (2*t*width + width) * b, true
		}
		return 8 * t * width, (2*t*width + 2*width) * b, true
	case "aten::scaled_dot_product_attention", "aten::_scaled_dot_product_flash_attention", "aten::_scaled_dot_product_efficient_attention", "aten::_scaled_dot_product_cudnn_attention":
		q, k, v := dim(0, 4), dim(1, 4), dim(2, 4)
		b := size(0)
		if q == nil || k == nil || v == nil || b == 0 || !positiveAll(q[0], q[1], q[2], q[3], k[1], k[2], v[3]) || k[0] != q[0] || v[0] != q[0] || k[3] != q[3] || v[2] != k[2] {
			return 0, 0, false
		}
		batch, heads, queries, depth := float64(q[0]), float64(q[1]), float64(q[2]), float64(q[3])
		kvHeads, keys, valueDepth := float64(k[1]), float64(k[2]), float64(v[3])
		flops := 2 * batch * heads * queries * keys * (depth + valueDepth)
		bytes := batch * (heads*queries*depth + kvHeads*keys*depth + kvHeads*keys*valueDepth + heads*queries*valueDepth) * b
		return flops, bytes, true
	case "vllm::outplace_fused_experts", "vllm::inplace_fused_experts":
		hidden, w1, w2, ids := dim(0, 2), dim(1, 3), dim(2, 3), dim(4, 2)
		activation, weightBytes := size(0), size(1)
		if hidden == nil || w1 == nil || w2 == nil || ids == nil || activation == 0 || weightBytes == 0 {
			return 0, 0, false
		}
		tokens, width := hidden[0], hidden[1]
		experts, up, down := w1[0], w1[1], w2[2]
		topK := ids[1]
		if !positiveAll(tokens, width, experts, up, down, topK) || w1[2] != width || w2[0] != experts || w2[1] != width || ids[0] != tokens {
			return 0, 0, false
		}
		routed := float64(min(experts, tokens*topK))
		t, h, k := float64(tokens), float64(width), float64(topK)
		flops := 2 * t * k * (float64(up)*h + h*float64(down))
		bytes := routed*(float64(up)*h+h*float64(down))*weightBytes + 2*t*h*activation
		return flops, bytes, true
	case "_moe_C::topk_softmax":
		weights, gating := dim(0, 2), dim(3, 2)
		gatingBytes := size(3)
		if weights == nil || gating == nil || gatingBytes == 0 || !positiveAll(gating[0], gating[1], weights[1]) || weights[0] != gating[0] {
			return 0, 0, false
		}
		t, e, k := float64(gating[0]), float64(gating[1]), float64(weights[1])
		return 3 * t * e, t*e*gatingBytes + 3*t*k*4, true
	}
	return 0, 0, false
}

// operators is kernel time per operator class, and each class's efficiency
// over the kernels whose host operator of that class recorded shapes. A
// kernel replayed from a CUDA graph has no host operator, so it is counted in
// time but never in efficiency.
func (p *prepared) operators(roofline gpuprofile.Roofline) []gpuprofile.OperatorTime {
	type totals struct {
		ns, covered  int64
		flops, bytes float64
	}
	byOperator := map[string]*totals{}
	counted := map[int]bool{}
	var all int64
	for _, kernel := range p.trace.Kernels {
		length := kernel.End - kernel.Start
		all += length
		if kernel.Memory {
			continue
		}
		class := p.operatorClass(kernel)
		if class == "" {
			continue
		}
		entry := byOperator[class]
		if entry == nil {
			entry = &totals{}
			byOperator[class] = entry
		}
		entry.ns += length
		_, launch, ok := p.launchOfKernel(kernel)
		if !ok {
			continue
		}
		chain := p.chains[launch]
		for index := len(chain) - 1; index >= 0; index-- {
			op := p.trace.Ops[chain[index]]
			if op.Operator == 0 {
				continue
			}
			if gpuprofile.Operators[op.Operator-1] == class && op.FLOPs > 0 && op.Bytes > 0 {
				entry.covered += length
				if !counted[chain[index]] {
					counted[chain[index]] = true
					entry.flops += op.FLOPs
					entry.bytes += op.Bytes
				}
			}
			break
		}
	}
	out := []gpuprofile.OperatorTime{}
	for _, class := range gpuprofile.Operators {
		entry := byOperator[class]
		if entry == nil || entry.ns <= 0 {
			continue
		}
		operator := gpuprofile.OperatorTime{Operator: class, KernelMS: ms(entry.ns), Share: shareOf(entry.ns, all), CoveredShare: shareOf(entry.covered, entry.ns)}
		switch {
		case operator.KernelMS <= 0:
			continue
		case !p.trace.ShapesRecorded:
			operator.Reason = "The trace recorded no operator shapes, so this operator's efficiency is unknown."
		case shareOf(entry.covered, entry.ns) < minOperatorCoverage:
			operator.Reason = fmt.Sprintf("Shapes cover %s of this operator's kernel time, under %s; kernels replayed from a CUDA graph have no host operator, so its efficiency is unknown.", percent(shareOf(entry.covered, entry.ns)), percent(minOperatorCoverage))
		case roofline.Peak == nil:
			operator.Reason = "The accelerator's peak compute and memory bandwidth are unknown, so this operator's efficiency is unknown."
		default:
			seconds := float64(entry.covered) / 1e9
			intensity := entry.flops / entry.bytes
			achievedTFLOPS := entry.flops / seconds / 1e12
			achievedTBps := entry.bytes / seconds / 1e12
			attainable := min(roofline.Peak.ComputeTFLOPS, intensity*roofline.Peak.MemoryTBps)
			bound := "compute"
			if intensity*roofline.Peak.MemoryTBps < roofline.Peak.ComputeTFLOPS {
				bound = "memory"
			}
			efficiency := round(achievedTFLOPS/attainable, 4)
			if efficiency <= 0 || efficiency > 4 || round(intensity, 3) <= 0 || round(achievedTFLOPS, 6) <= 0 || round(achievedTBps, 6) <= 0 {
				operator.Reason = "This operator's recorded shapes give no usable throughput, so its efficiency is unknown."
				break
			}
			operator.Intensity, operator.AchievedTFLOPS, operator.AchievedTBps = pointer(round(intensity, 3)), pointer(round(achievedTFLOPS, 6)), pointer(round(achievedTBps, 6))
			operator.Bound, operator.Efficiency = pointer(bound), pointer(efficiency)
		}
		out = append(out, operator)
	}
	return out
}

func (p *prepared) operatorClass(kernel Kernel) string {
	return OperatorClass(p.trace.Names[kernel.Name])
}
