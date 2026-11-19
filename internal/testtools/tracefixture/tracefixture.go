// Package tracefixture writes small, deterministic GPU profiler traces shaped
// like the files vLLM and SGLang (PyTorch profiler / Kineto), rocprofv3, and
// Nsight Systems write. They are generated samples, not measurements: kernel
// and operator names follow the upstream libraries, and every time is made
// up to exercise one analysis path. Never present their numbers as results.
package tracefixture

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// SampleBase is the fixed clock of the committed sample files.
var SampleBase = time.Date(2026, 11, 17, 9, 0, 0, 0, time.UTC)

type event map[string]any

type writer struct {
	events      []event
	correlation int
	external    int
	pid         int
}

func (w *writer) next() int { w.correlation++; return w.correlation }

func (w *writer) op(category, name string, tid int, start, duration float64, args map[string]any) {
	if args == nil {
		args = map[string]any{}
	}
	w.external++
	args["External id"] = w.external
	w.events = append(w.events, event{"ph": "X", "cat": category, "name": name, "pid": w.pid, "tid": tid, "ts": round(start), "dur": round(duration), "args": args})
}

// launch writes one host launch call and its kernels, back to back on the
// GPU from gpuStart. It returns when the last kernel ends.
func (w *writer) launch(api string, tid int, hostStart, hostDuration float64, device, stream int, gpuStart float64, kernels []kernel) float64 {
	correlation := w.next()
	w.events = append(w.events, event{"ph": "X", "cat": "cuda_runtime", "name": api, "pid": w.pid, "tid": tid, "ts": round(hostStart), "dur": round(hostDuration),
		"args": map[string]any{"cbid": 211, "correlation": correlation, "External id": w.external}})
	at := gpuStart
	for _, current := range kernels {
		category := "kernel"
		if current.memory {
			category = "gpu_memcpy"
		}
		w.events = append(w.events, event{"ph": "X", "cat": category, "name": current.name, "pid": device, "tid": stream, "ts": round(at), "dur": round(current.duration),
			"args": map[string]any{"queued": 0, "device": device, "context": 1, "stream": stream, "correlation": correlation, "registers per thread": 128, "grid": []int{132, 1, 1}, "block": []int{384, 1, 1}}})
		at += current.duration + current.gapAfter
	}
	return at
}

type kernel struct {
	name     string
	duration float64
	gapAfter float64
	memory   bool
}

func round(value float64) float64 { return float64(int64(value*1000+0.5)) / 1000 }

func kinetoFile(base time.Time, device string, devices []int, events []event, shapes bool) []byte {
	properties := make([]map[string]any, 0, len(devices))
	for _, id := range devices {
		properties = append(properties, map[string]any{"id": id, "name": device, "totalGlobalMem": 85_520_809_984, "computeMajor": 9, "computeMinor": 0, "maxThreadsPerBlock": 1024, "numSms": 132})
	}
	var buffer bytes.Buffer
	buffer.WriteString("{\n")
	writeKey := func(key string, value any) {
		encoded, _ := json.Marshal(value)
		fmt.Fprintf(&buffer, "  %q: %s,\n", key, encoded)
	}
	writeKey("schemaVersion", 1)
	writeKey("deviceProperties", properties)
	writeKey("cupti_version", 26)
	writeKey("cuda_runtime_version", 12090)
	writeKey("record_shapes", map[bool]int{true: 1, false: 0}[shapes])
	writeKey("with_stack", 0)
	writeKey("trace_id", "0123456789ABCDEF0123456789ABCDEF")
	writeKey("displayTimeUnit", "ms")
	writeKey("baseTimeNanoseconds", base.UnixNano())
	buffer.WriteString("  \"traceEvents\": [\n")
	for index, current := range events {
		encoded, _ := json.Marshal(current)
		buffer.WriteString("  ")
		buffer.Write(encoded)
		if index < len(events)-1 {
			buffer.WriteString(",")
		}
		buffer.WriteString("\n")
	}
	buffer.WriteString("  ],\n  \"traceName\": \"sample.pt.trace.json\"\n}\n")
	return gzipBytes(buffer.Bytes())
}

func gzipBytes(data []byte) []byte {
	var buffer bytes.Buffer
	compressed, _ := gzip.NewWriterLevel(&buffer, gzip.BestCompression)
	_, _ = compressed.Write(data)
	_ = compressed.Close()
	return buffer.Bytes()
}

const (
	gemmPrefill = "sm90_xmma_gemm_bf16bf16_bf16f32_f32_tn_n_tilesize128x256x64_warpgroupsize2x1x1_execute_segment_k_off_kernel__5x_cublas"
	gemmDecode  = "sm90_xmma_gemm_bf16bf16_bf16f32_f32_tn_n_tilesize64x64x64_warpgroupsize1x1x1_execute_segment_k_off_kernel__5x_cublas"
	elementwise = "void at::native::vectorized_elementwise_kernel<4, at::native::FillFunctor<long>, std::array<char*, 1ul> >(int, at::native::FillFunctor<long>, std::array<char*, 1ul>)"
)

// VLLMSingleGPU is a vLLM-shaped torch-profiler trace of one H100: two eager
// prefill steps with recorded matrix-multiply shapes, then forty decode steps
// replayed from a CUDA graph, each started only after the host prepared the
// next batch. ProfilerStep#N marks every step, as vLLM writes it when it
// profiles on a schedule. It holds request-like text in fields the parser
// must ignore.
func VLLMSingleGPU(base time.Time) []byte {
	w := &writer{pid: 4242}
	const tid, device, stream = 4242, 0, 7
	host := 100.0
	gpuFree := 0.0
	step := 0
	shapes := func(m, k, n int) map[string]any {
		return map[string]any{"Input Dims": [][]int{{m, k}, {k, n}}, "Input type": []string{"c10::BFloat16", "c10::BFloat16"},
			"Concrete Inputs": []string{"", ""}, "Input Strides": [][]int{{k, 1}, {1, k}}}
	}
	for ; step < 2; step++ {
		stepStart := host
		layers := []struct {
			op      string
			args    map[string]any
			kernels []kernel
		}{
			{"_C::rms_norm", nil, []kernel{{"void vllm::rms_norm_kernel<c10::BFloat16, 8, 1>(c10::BFloat16*, c10::BFloat16 const*, long, c10::BFloat16 const*, float, int, int)", 15, 0, false}}},
			{"aten::mm", shapes(2048, 4096, 6144), []kernel{{gemmPrefill, 180, 0, false}}},
			{"vllm::unified_attention_with_output", nil, []kernel{{"void flashinfer::BatchPrefillWithPagedKVCacheKernel<128u, 4u, 1u>(flashinfer::BatchPrefillPagedParams)", 120, 0, false}}},
			{"aten::mm", shapes(2048, 4096, 4096), []kernel{{gemmPrefill, 120, 0, false}}},
			{"aten::mm", shapes(2048, 4096, 28672), []kernel{{gemmPrefill, 800, 0, false}}},
			{"_C::silu_and_mul", nil, []kernel{{"void vllm::act_and_mul_kernel<c10::BFloat16, &vllm::silu_kernel<c10::BFloat16>, true>(c10::BFloat16*, c10::BFloat16 const*, int)", 40, 0, false}}},
			{"aten::mm", shapes(2048, 14336, 4096), []kernel{{gemmPrefill, 400, 0, false}}},
		}
		var stepEnd float64
		for layer := 0; layer < 4; layer++ {
			for _, current := range layers {
				w.op("cpu_op", current.op, tid, host, 9, current.args)
				gpuStart := maxFloat(gpuFree, host+3)
				gpuFree = w.launch("cudaLaunchKernel", tid, host+1, 5, device, stream, gpuStart, current.kernels)
				host += 10
				stepEnd = gpuFree
			}
		}
		w.op("user_annotation", fmt.Sprintf("ProfilerStep#%d", step), tid, stepStart, host-stepStart, nil)
		host = maxFloat(host, stepEnd) + 900
	}
	decodeLayer := []kernel{
		{"void vllm::rms_norm_kernel<c10::BFloat16, 8, 1>(c10::BFloat16*, c10::BFloat16 const*, long, c10::BFloat16 const*, float, int, int)", 8, 1, false},
		{gemmDecode, 60, 1, false},
		{"void vllm::rotary_embedding_kernel<c10::BFloat16, true>(long const*, c10::BFloat16*, c10::BFloat16*, c10::BFloat16 const*, int, long, long, int, int, int, int)", 6, 1, false},
		{"void flashinfer::BatchDecodeWithPagedKVCacheKernel<128u, 4u, 8u, 16u, 1u>(flashinfer::BatchDecodeParams)", 45, 1, false},
		{gemmDecode, 40, 1, false},
		{"void vllm::rms_norm_kernel<c10::BFloat16, 8, 1>(c10::BFloat16*, c10::BFloat16 const*, long, c10::BFloat16 const*, float, int, int)", 8, 1, false},
		{gemmDecode, 210, 1, false},
		{"void vllm::act_and_mul_kernel<c10::BFloat16, &vllm::silu_kernel<c10::BFloat16>, true>(c10::BFloat16*, c10::BFloat16 const*, int)", 10, 1, false},
		{gemmDecode, 110, 1, false},
	}
	for ; step < 42; step++ {
		stepStart := host
		w.op("cpu_op", "aten::copy_", tid, host, 12, map[string]any{"Concrete Inputs": []string{"SECRET-PROMPT-TEXT", ""}})
		gpuFree = w.launch("cudaMemcpyAsync", tid, host+2, 6, device, stream, maxFloat(gpuFree, host+5), []kernel{{"Memcpy HtoD (Pageable -> Device)", 4, 0, true}})
		host += 20
		for small := 0; small < 6; small++ {
			w.op("cpu_op", "aten::fill_", tid, host, 8, nil)
			gpuFree = w.launch("cudaLaunchKernel", tid, host+1, 5, device, stream, maxFloat(gpuFree, host+4), []kernel{{elementwise, 2, 0, false}})
			host += 9
		}
		host += 1150 // the host prepares the batch while the GPU waits
		var graph []kernel
		for layer := 0; layer < 4; layer++ {
			graph = append(graph, decodeLayer...)
		}
		w.op("cpu_op", "vllm::cuda_graph_replay", tid, host, 16, nil)
		gpuFree = w.launch("cudaGraphLaunch", tid, host+2, 12, device, stream, maxFloat(gpuFree, host+10), graph)
		host += 20
		w.op("cpu_op", "vllm::sample", tid, host, 8, nil)
		gpuFree = w.launch("cudaLaunchKernel", tid, host+1, 5, device, stream, maxFloat(gpuFree, host+4), []kernel{{"void flashinfer::sampling::TopKTopPSamplingFromProbKernel<1024u, (cub::BlockScanAlgorithm)2, (cub::BlockReduceAlgorithm)0, 4u, false, float, int>(float*, int*, int*, unsigned int, float const*, int const*, float const*, unsigned int, unsigned long, unsigned long)", 30, 0, false}})
		w.op("user_annotation", fmt.Sprintf("ProfilerStep#%d", step), tid, stepStart, maxFloat(host+10, gpuFree)-stepStart, nil)
		host = maxFloat(host+10, gpuFree) + 40
	}
	// Noise the parser must skip: Python frames, flow events, metadata.
	w.events = append(w.events,
		event{"ph": "X", "cat": "python_function", "name": "vllm/v1/worker/gpu_model_runner.py(1234): execute_model", "pid": 4242, "tid": tid, "ts": 50.0, "dur": 10.0, "args": map[string]any{"Python id": 1}},
		event{"ph": "s", "id": 1, "pid": 4242, "tid": tid, "ts": 101.0, "cat": "ac2g", "name": "ac2g"},
		event{"ph": "M", "name": "thread_name", "pid": 4242, "tid": tid, "args": map[string]any{"name": "thread 4242 (python3)"}},
		event{"ph": "X", "cat": "user_annotation", "name": "request SECRET-REQUEST-ID", "pid": 4242, "tid": tid, "ts": 100.0, "dur": 5.0, "args": map[string]any{}},
	)
	return kinetoFile(base, "NVIDIA H100 80GB HBM3", []int{0}, w.events, true)
}

// VLLMMoE is a vLLM-shaped torch-profiler trace of one H200 serving a
// mixture-of-experts model with DeepSeek-V3's layer shapes: hidden size 7168,
// 256 experts with a 2048 intermediate size, eight routed per token, FP8
// expert weights. Two eager prefill steps record operator shapes for the
// fused residual norm, the router's top-k softmax, and the fused experts; a
// few decode steps replay from a CUDA graph, so their kernels have no host
// operator. Attention runs through vLLM's paged attention, whose shapes do
// not carry its cost. Every time is made up.
func VLLMMoE(base time.Time) []byte {
	w := &writer{pid: 5151}
	const tid, device, stream = 5151, 0, 7
	const tokens, hidden, experts, intermediate, topK = 2048, 7168, 256, 2048, 8
	bf16 := "c10::BFloat16"
	normArgs := map[string]any{"Input Dims": [][]int{{tokens, hidden}, {tokens, hidden}, {hidden}, {}}, "Input type": []string{bf16, bf16, bf16, "double"}}
	routerArgs := map[string]any{"Input Dims": [][]int{{tokens, topK}, {tokens, topK}, {tokens, topK}, {tokens, experts}}, "Input type": []string{"float", "int", "int", "float"}}
	expertArgs := map[string]any{
		"Input Dims": [][]int{{tokens, hidden}, {experts, 2 * intermediate, hidden}, {experts, hidden, intermediate}, {tokens, topK}, {tokens, topK}},
		"Input type": []string{bf16, "c10::Float8_e4m3fn", "c10::Float8_e4m3fn", "float", "int"},
	}
	norm := "void vllm::fused_add_rms_norm_kernel<c10::BFloat16, 8>(c10::BFloat16*, c10::BFloat16*, c10::BFloat16 const*, float, int, int)"
	router := "void vllm::moe::topkGatingSoftmax<8, 256, 4, 16, 32, int, float>(float const*, bool const*, float*, int, int*, int*, int, int, int)"
	align := "void vllm::moe::moe_align_block_size_kernel<int>(int const*, int*, int*, int*, int, int, int, int, unsigned long, int*)"
	expertsKernel := "fused_moe_kernel"
	attention := "void flashinfer::BatchPrefillWithPagedKVCacheKernel<128u, 4u, 1u>(flashinfer::BatchPrefillPagedParams)"
	host, gpuFree, step := 100.0, 0.0, 0
	for ; step < 2; step++ {
		stepStart := host
		layers := []struct {
			op      string
			args    map[string]any
			kernels []kernel
		}{
			{"_C::fused_add_rms_norm", normArgs, []kernel{{norm, 50, 0, false}}},
			{"vllm::unified_attention_with_output", nil, []kernel{{attention, 400, 0, false}}},
			{"_moe_C::topk_softmax", routerArgs, []kernel{{router, 20, 0, false}}},
			{"_moe_C::moe_align_block_size", nil, []kernel{{align, 30, 0, false}}},
			{"vllm::outplace_fused_experts", expertArgs, []kernel{{expertsKernel, 3200, 0, false}, {expertsKernel, 3300, 0, false}}},
		}
		var stepEnd float64
		for layer := 0; layer < 3; layer++ {
			for _, current := range layers {
				w.op("cpu_op", current.op, tid, host, 9, current.args)
				gpuStart := maxFloat(gpuFree, host+3)
				gpuFree = w.launch("cudaLaunchKernel", tid, host+1, 5, device, stream, gpuStart, current.kernels)
				host += 10
				stepEnd = gpuFree
			}
		}
		w.op("user_annotation", fmt.Sprintf("ProfilerStep#%d", step), tid, stepStart, host-stepStart, nil)
		host = maxFloat(host, stepEnd) + 600
	}
	decodeLayer := []kernel{
		{norm, 6, 1, false},
		{"void flashinfer::BatchDecodeWithPagedKVCacheKernel<128u, 4u, 8u, 16u, 1u>(flashinfer::BatchDecodeParams)", 60, 1, false},
		{router, 8, 1, false},
		{align, 10, 1, false},
		{expertsKernel, 900, 1, false},
	}
	for ; step < 6; step++ {
		stepStart := host
		var graph []kernel
		for layer := 0; layer < 3; layer++ {
			graph = append(graph, decodeLayer...)
		}
		w.op("cpu_op", "vllm::cuda_graph_replay", tid, host, 16, nil)
		gpuFree = w.launch("cudaGraphLaunch", tid, host+2, 12, device, stream, maxFloat(gpuFree, host+10), graph)
		w.op("user_annotation", fmt.Sprintf("ProfilerStep#%d", step), tid, stepStart, maxFloat(host+20, gpuFree)-stepStart, nil)
		host = maxFloat(host+20, gpuFree) + 40
	}
	return kinetoFile(base, "NVIDIA H200", []int{device}, w.events, true)
}

// SGLangTP2 is an SGLang-shaped trace pair of a tensor-parallel decode on two
// H100s: one file per rank, CUDA graph replays, no step annotations, and an
// NCCL all-reduce on its own stream that overlaps compute only at its edge.
func SGLangTP2(base time.Time) [][]byte {
	files := make([][]byte, 0, 2)
	for rank := 0; rank < 2; rank++ {
		w := &writer{pid: 5100 + rank}
		tid := 5100 + rank
		host, gpuFree := 50.0+float64(rank), 0.0
		for step := 0; step < 30; step++ {
			// The scheduler queues the next graph before the GPU finishes, and
			// the replay starts 120 µs after the previous step ends.
			start := maxFloat(gpuFree+120, host+8)
			if step == 0 {
				start = host + 8
			}
			w.op("cpu_op", "sglang::cuda_graph_replay", tid, host, 14, nil)
			correlation := w.next()
			w.events = append(w.events, event{"ph": "X", "cat": "cuda_runtime", "name": "cudaGraphLaunch", "pid": w.pid, "tid": tid, "ts": round(host + 1), "dur": 11.0,
				"args": map[string]any{"cbid": 311, "correlation": correlation, "External id": w.external}})
			at := start
			for layer := 0; layer < 4; layer++ {
				for _, current := range []kernel{
					{"void flashinfer::norm::FusedAddRMSNormKernel<8u, __nv_bfloat16>(__nv_bfloat16*, __nv_bfloat16*, __nv_bfloat16*, unsigned int, unsigned int, unsigned int, float, float)", 6, 1, false},
					{"nvjet_tst_64x8_64x16_4x1_v_bz_TNT", 30, 1, false},
					{"void flashinfer::BatchDecodeWithPagedKVCacheKernel<128u, 4u, 8u, 16u, 1u>(flashinfer::BatchDecodeParams)", 25, 1, false},
					{"nvjet_tst_64x8_64x16_4x1_v_bz_TNT", 20, 1, false},
				} {
					w.events = append(w.events, event{"ph": "X", "cat": "kernel", "name": current.name, "pid": rank, "tid": 7, "ts": round(at), "dur": current.duration,
						"args": map[string]any{"device": rank, "stream": 7, "correlation": correlation}})
					at += current.duration + current.gapAfter
				}
				// All-reduce on the NCCL stream; the next layer's norm overlaps its last 10 µs.
				w.events = append(w.events, event{"ph": "X", "cat": "kernel", "name": "ncclDevKernel_AllReduce_Sum_bf16_RING_LL(ncclDevKernelArgsStorage<4096ul>)", "pid": rank, "tid": 21, "ts": round(at), "dur": 40.0,
					"args": map[string]any{"device": rank, "stream": 21, "correlation": correlation}})
				at += 30
			}
			at += 10
			gpuFree = at
			host = at - 100
		}
		files = append(files, kinetoFile(base.Add(time.Duration(rank)*time.Microsecond), "NVIDIA H100 80GB HBM3", []int{rank}, w.events, false))
	}
	return files
}

// RocprofV3 is rocprofv3 CSV output (kernel, HIP API, and memory copy traces)
// shaped like a vLLM decode on one MI300X agent.
func RocprofV3() (kernels, hipAPI, copies []byte) {
	var k, h, c strings.Builder
	k.WriteString(`"Kind","Agent_Id","Queue_Id","Stream_Id","Thread_Id","Dispatch_Id","Kernel_Id","Kernel_Name","Correlation_Id","Start_Timestamp","End_Timestamp","LDS_Block_Size","Scratch_Size","VGPR_Count","Accum_VGPR_Count","SGPR_Count","Workgroup_Size_X","Workgroup_Size_Y","Workgroup_Size_Z","Grid_Size_X","Grid_Size_Y","Grid_Size_Z"` + "\n")
	h.WriteString(`"Domain","Function","Process_Id","Thread_Id","Correlation_Id","Start_Timestamp","End_Timestamp"` + "\n")
	c.WriteString(`"Kind","Direction","Source_Agent_Id","Destination_Agent_Id","Correlation_Id","Start_Timestamp","End_Timestamp"` + "\n")
	const origin = int64(1_000_000_000)
	at, host := origin, origin-20_000
	dispatch, correlation := 0, 0
	names := []struct {
		name string
		ns   int64
	}{
		{"_ZN4vllm15rms_norm_kernelIN3c108BFloat16ELi8ELi1EEEvPT_PKS3_lS6_fii", 9_000},
		{"Cijk_Alik_Bljk_BBS_BH_MT64x64x64_MI16x16x16x1_SN_1LDSB0_APM1_ABV0_ACED0_AF0EM1_AF1EM1_AMAS3_ASE_ASGT_ASLT_ASAE01_ASCE01_ASEM1_AAC0_BL1_BS1", 70_000},
		{"_Z38paged_attention_ll4mi_QKV_mfma16_kernelI14__hip_bfloat16S0_Li4ELi128ELi16ELi1ELi16EEvPKT_PKT0_S7_ifPKiS9_S9_iPKfiiiPfSB_PS2_SC_fff", 52_000},
		{"Cijk_Alik_Bljk_BBS_BH_MT64x64x64_MI16x16x16x1_SN_1LDSB0_APM1_ABV0_ACED0_AF0EM1_AF1EM1_AMAS3_ASE_ASGT_ASLT_ASAE01_ASCE01_ASEM1_AAC0_BL1_BS1", 250_000},
		{"_ZN4vllm18act_and_mul_kernelIN3c108BFloat16EXadL_ZNS_11silu_kernelIS2_EET_RKS4_EELb1EEEvPS4_PS5_i", 12_000},
	}
	for step := 0; step < 20; step++ {
		correlation++
		fmt.Fprintf(&c, `"MEMORY_COPY","MEMORY_COPY_HOST_TO_DEVICE",1,4,%d,%d,%d`+"\n", correlation, at, at+5_000)
		fmt.Fprintf(&h, `"HIP_RUNTIME_API","hipMemcpyAsync",%d,%d,%d,%d,%d`+"\n", 7000, 7001, correlation, host, host+7_000)
		at += 6_000
		for layer := 0; layer < 4; layer++ {
			for _, current := range names {
				correlation++
				dispatch++
				fmt.Fprintf(&h, `"HIP_RUNTIME_API","hipModuleLaunchKernel",%d,%d,%d,%d,%d`+"\n", 7000, 7001, correlation, host, host+6_000)
				start := maxInt(at, host+8_000)
				fmt.Fprintf(&k, `"KERNEL_DISPATCH",4,1,1,7001,%d,%d,"%s",%d,%d,%d,0,0,64,0,32,256,1,1,304,1,1`+"\n", dispatch, 10+dispatch%5, current.name, correlation, start, start+current.ns)
				at = start + current.ns + 2_000
				host += 7_000
			}
		}
		host = at + 400_000
		at = host
	}
	return []byte(k.String()), []byte(h.String()), []byte(c.String())
}

// NsysCSV is `nsys stats --report cuda_gpu_trace,cuda_api_trace --format csv`
// output shaped like an eager (no CUDA graph) decode on one L40S, where tiny
// kernels finish faster than the host launches them.
func NsysCSV() (gpuTrace, apiTrace []byte) {
	var g, a strings.Builder
	g.WriteString("Start (ns),Duration (ns),CorrId,GrdX,GrdY,GrdZ,BlkX,BlkY,BlkZ,Reg/Trd,StcSMem (MB),DymSMem (MB),Bytes (MB),Throughput (MBps),SrcMemKd,DstMemKd,Device,Ctx,GreenCtx,Strm,Name\n")
	a.WriteString("Start (ns),Duration (ns),Name,Result,CorrID,Pid,Tid,T-Pri,Thread Name\n")
	host, gpu := int64(500_000), int64(0)
	correlation := 100
	for step := 0; step < 25; step++ {
		for index := 0; index < 40; index++ {
			correlation++
			name, duration := "void at::native::elementwise_kernel<128, 2, at::native::gpu_kernel_impl<at::native::CUDAFunctor_add<c10::BFloat16> >(at::TensorIteratorBase&, at::native::CUDAFunctor_add<c10::BFloat16> const&)::{lambda(int)#1}>(int, at::native::gpu_kernel_impl<at::native::CUDAFunctor_add<c10::BFloat16> >(at::TensorIteratorBase&, at::native::CUDAFunctor_add<c10::BFloat16> const&)::{lambda(int)#1})", int64(3_000)
			if index%5 == 0 {
				name, duration = "ampere_bf16_s16816gemm_bf16_64x64_ldg8_f2f_stages_64x5_tn", 45_000
			}
			fmt.Fprintf(&a, "%d,%d,cudaLaunchKernel_v7000,0,%d,9001,9001,0,python3\n", host, 7_000, correlation)
			start := maxInt(gpu, host+5_000)
			fmt.Fprintf(&g, "%d,%d,%d,128,1,1,256,1,1,64,0.000,0.000,,,,,NVIDIA L40S (0),1,,7,%q\n", start, duration, correlation, name)
			gpu = start + duration
			host += 8_000
		}
		host += 200_000
	}
	return []byte(g.String()), []byte(a.String())
}

func maxFloat(left, right float64) float64 {
	if left > right {
		return left
	}
	return right
}

func maxInt(left, right int64) int64 {
	if left > right {
		return left
	}
	return right
}
