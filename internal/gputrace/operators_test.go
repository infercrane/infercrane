package gputrace

import (
	"math"
	"path/filepath"
	"strings"
	"testing"

	"github.com/infercrane/infercrane/internal/gpuprofile"
)

func TestOperatorClassNamesMoEBeforeTheGenericCategories(t *testing.T) {
	for name, want := range map[string]string{
		"fused_moe_kernel": gpuprofile.OperatorMoEExperts,
		"void vllm::moe::topkGatingSoftmax<8, 256, 4, 16, 32, int, float>(float const*)": gpuprofile.OperatorMoERouting,
		"void vllm::moe::moe_align_block_size_kernel<int>(int const*)":                   gpuprofile.OperatorMoERouting,
		"void flashinfer::BatchDecodeWithPagedKVCacheKernel<128u, 4u>(params)":           gpuprofile.OperatorAttention,
		"void vllm::fused_add_rms_norm_kernel<c10::BFloat16, 8>(c10::BFloat16*)":         gpuprofile.OperatorNorm,
		"sm90_xmma_gemm_bf16bf16_bf16f32_f32_tn_n":                                       "",
		"ncclDevKernel_AllReduce_Sum_bf16_RING_LL":                                       "",
		"void at::native::(anonymous namespace)::cunn_SoftMaxForward<4, float>(float*)":  "",
	} {
		if got := OperatorClass(name); got != want {
			t.Errorf("%s: %q, want %q", name, got, want)
		}
	}
}

func TestOperatorShapesCountTheirMinimumTraffic(t *testing.T) {
	bf16, fp8 := "c10::BFloat16", "c10::Float8_e4m3fn"
	cases := []struct {
		name         string
		dims         [][]int64
		types        []string
		flops, bytes float64
	}{
		// out, input, weight: one read and one write of the activations, and the weight.
		{"_C::rms_norm", [][]int64{{8, 4096}, {8, 4096}, {4096}}, []string{bf16, bf16, bf16}, 4 * 8 * 4096, (2*8*4096 + 4096) * 2},
		// input and residual are read and both written back in place.
		{"_C::fused_add_rms_norm", [][]int64{{8, 4096}, {8, 4096}, {4096}}, []string{bf16, bf16, bf16}, 5 * 8 * 4096, (4*8*4096 + 4096) * 2},
		{"aten::layer_norm", [][]int64{{2, 4, 1024}, {}, {1024}, {1024}}, []string{bf16, "", bf16, bf16}, 8 * 8 * 1024, (2*8*1024 + 2*1024) * 2},
		// Grouped-query attention: 32 query heads over 8 key-value heads.
		{"aten::scaled_dot_product_attention", [][]int64{{1, 32, 512, 128}, {1, 8, 512, 128}, {1, 8, 512, 128}}, []string{bf16, bf16, bf16},
			2 * 32 * 512 * 512 * 256, (32*512*128 + 8*512*128 + 8*512*128 + 32*512*128) * 2},
		// 4 tokens × 2 experts each touch at most 8 of 16 experts; FP8 weights.
		{"vllm::outplace_fused_experts", [][]int64{{4, 64}, {16, 32, 64}, {16, 64, 16}, {4, 2}, {4, 2}}, []string{bf16, fp8, fp8, "float", "int"},
			2 * 4 * 2 * (32*64 + 64*16), 8*(32*64+64*16)*1 + 2*4*64*2},
		{"_moe_C::topk_softmax", [][]int64{{4, 2}, {4, 2}, {4, 2}, {4, 16}}, []string{"float", "int", "int", "float"}, 3 * 4 * 16, 4*16*4 + 3*4*2*4},
	}
	for _, current := range cases {
		flops, bytes, ok := operatorShape(current.name, current.dims, current.types)
		if !ok || flops != current.flops || bytes != current.bytes {
			t.Errorf("%s: flops=%v bytes=%v ok=%v, want %v and %v", current.name, flops, bytes, ok, current.flops, current.bytes)
		}
	}
}

func TestOperatorShapesThatDoNotCarryTheCostAreUnknown(t *testing.T) {
	bf16 := "c10::BFloat16"
	for _, current := range []struct {
		name  string
		dims  [][]int64
		types []string
	}{
		// Paged attention keeps context lengths in metadata, not shapes.
		{"vllm::unified_attention_with_output", [][]int64{{8, 4096}, {8, 1024}, {8, 1024}, {8, 4096}}, []string{bf16, bf16, bf16, bf16}},
		{"_C::rms_norm", [][]int64{{8, 4096}, {8, 4096}, {2048}}, []string{bf16, bf16, bf16}},
		{"_C::rms_norm", [][]int64{{8, 4096}, {8, 4096}, {4096}}, []string{bf16, "c10::complex<float>", bf16}},
		{"vllm::outplace_fused_experts", [][]int64{{4, 64}, {16, 32, 63}, {16, 64, 16}, {4, 2}, {4, 2}}, []string{bf16, bf16, bf16, "float", "int"}},
		{"aten::scaled_dot_product_attention", [][]int64{{1, 32, 512, 128}, {2, 8, 512, 128}, {1, 8, 512, 128}}, []string{bf16, bf16, bf16}},
	} {
		if flops, bytes, ok := operatorShape(current.name, current.dims, current.types); ok {
			t.Errorf("%s %v: flops=%v bytes=%v, want unknown", current.name, current.dims, flops, bytes)
		}
	}
}

// The MoE sample's experts are covered by eager prefill and measured; its
// routing is mostly replayed from a CUDA graph and stays unknown.
func TestMoESampleMeasuresExpertsAndNamesWhatItCannot(t *testing.T) {
	trace, err := ParseFiles([]string{filepath.Join("testdata", "vllm-moe-h200")}, "")
	if err != nil {
		t.Fatal(err)
	}
	analysis, err := Analyze(trace, Options{Engine: "vllm"})
	if err != nil {
		t.Fatal(err)
	}
	byOperator := map[string]gpuprofile.OperatorTime{}
	for _, operator := range analysis.Operators {
		byOperator[operator.Operator] = operator
	}
	experts := byOperator[gpuprofile.OperatorMoEExperts]
	if experts.Efficiency == nil || experts.Bound == nil || *experts.Bound != "memory" || experts.CoveredShare < minOperatorCoverage {
		t.Fatalf("experts = %+v", experts)
	}
	// Achieved over attainable, recomputed from the reported throughput.
	peak := analysis.Roofline.Peak
	attainable := math.Min(peak.ComputeTFLOPS, *experts.Intensity*peak.MemoryTBps)
	if math.Abs(*experts.Efficiency-*experts.AchievedTFLOPS/attainable) > 1e-3 {
		t.Fatalf("efficiency %v is not achieved/attainable %v", *experts.Efficiency, *experts.AchievedTFLOPS/attainable)
	}
	routing := byOperator[gpuprofile.OperatorMoERouting]
	if routing.Efficiency != nil || !strings.Contains(routing.Reason, "CUDA graph") {
		t.Fatalf("routing = %+v", routing)
	}
	attention := byOperator[gpuprofile.OperatorAttention]
	if attention.Efficiency != nil || attention.CoveredShare != 0 {
		t.Fatalf("paged attention = %+v", attention)
	}
	total := 0.0
	for _, operator := range analysis.Operators {
		total += operator.Share
	}
	if total > 1.0001 {
		t.Fatalf("operator shares sum to %v", total)
	}
}

func TestOperatorEfficiencyNeedsAPeak(t *testing.T) {
	trace, err := ParseFiles([]string{filepath.Join("testdata", "vllm-moe-h200")}, "")
	if err != nil {
		t.Fatal(err)
	}
	p := prepare(trace)
	for _, operator := range p.operators(gpuprofile.Roofline{Position: "unknown", Reason: "unknown accelerator"}) {
		if operator.Efficiency != nil || operator.Reason == "" {
			t.Fatalf("%+v measured without a peak", operator)
		}
	}
}
