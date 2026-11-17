package gputrace

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/infercrane/infercrane/internal/gpuprofile"
)

// minRooflineCoverage is the share of matrix-multiply time that needs
// recorded shapes before the analysis names a roofline position.
const minRooflineCoverage = 0.5

// peakEntry is a vendor datasheet peak: dense (no sparsity) 16-bit tensor
// throughput and memory bandwidth. They are not measurements, and an
// accelerator missing here leaves the roofline unknown.
type peakEntry struct {
	pattern *regexp.Regexp
	peak    gpuprofile.Peak
}

var peaks = []peakEntry{
	{regexp.MustCompile(`(?i)h200 nvl`), gpuprofile.Peak{ComputeTFLOPS: 835.5, Precision: "BF16 dense tensor", MemoryTBps: 4.8, Source: "NVIDIA H200 datasheet (half the sparse figure)"}},
	{regexp.MustCompile(`(?i)h200`), gpuprofile.Peak{ComputeTFLOPS: 989.5, Precision: "BF16 dense tensor", MemoryTBps: 4.8, Source: "NVIDIA H200 datasheet, SXM (half the sparse figure)"}},
	{regexp.MustCompile(`(?i)h100 nvl`), gpuprofile.Peak{ComputeTFLOPS: 835.5, Precision: "BF16 dense tensor", MemoryTBps: 3.9, Source: "NVIDIA H100 datasheet, NVL (half the sparse figure)"}},
	{regexp.MustCompile(`(?i)h100.*pcie`), gpuprofile.Peak{ComputeTFLOPS: 756.5, Precision: "BF16 dense tensor", MemoryTBps: 2.0, Source: "NVIDIA H100 datasheet, PCIe (half the sparse figure)"}},
	{regexp.MustCompile(`(?i)h100.*(hbm3|sxm)`), gpuprofile.Peak{ComputeTFLOPS: 989.5, Precision: "BF16 dense tensor", MemoryTBps: 3.35, Source: "NVIDIA H100 datasheet, SXM (half the sparse figure)"}},
	{regexp.MustCompile(`(?i)a100.*80gb.*pcie|a100.*pcie.*80gb`), gpuprofile.Peak{ComputeTFLOPS: 312, Precision: "BF16 dense tensor", MemoryTBps: 1.935, Source: "NVIDIA A100 datasheet, 80GB PCIe"}},
	{regexp.MustCompile(`(?i)a100.*sxm.*80gb`), gpuprofile.Peak{ComputeTFLOPS: 312, Precision: "BF16 dense tensor", MemoryTBps: 2.039, Source: "NVIDIA A100 datasheet, 80GB SXM"}},
	{regexp.MustCompile(`(?i)a100.*40gb`), gpuprofile.Peak{ComputeTFLOPS: 312, Precision: "BF16 dense tensor", MemoryTBps: 1.555, Source: "NVIDIA A100 datasheet, 40GB"}},
	{regexp.MustCompile(`(?i)l40s`), gpuprofile.Peak{ComputeTFLOPS: 362.05, Precision: "BF16 dense tensor", MemoryTBps: 0.864, Source: "NVIDIA L40S datasheet (half the sparse figure)"}},
	{regexp.MustCompile(`(?i)(^|\s)l4$`), gpuprofile.Peak{ComputeTFLOPS: 121, Precision: "BF16 dense tensor", MemoryTBps: 0.3, Source: "NVIDIA L4 datasheet (half the sparse figure)"}},
	{regexp.MustCompile(`(?i)(^|\s)a10$`), gpuprofile.Peak{ComputeTFLOPS: 125, Precision: "BF16 dense tensor", MemoryTBps: 0.6, Source: "NVIDIA A10 datasheet (half the sparse figure)"}},
	{regexp.MustCompile(`(?i)rtx 4090`), gpuprofile.Peak{ComputeTFLOPS: 165.2, Precision: "BF16 dense tensor, FP32 accumulate", MemoryTBps: 1.008, Source: "NVIDIA Ada Lovelace architecture whitepaper, GeForce RTX 4090"}},
	{regexp.MustCompile(`(?i)mi325x`), gpuprofile.Peak{ComputeTFLOPS: 1307.4, Precision: "BF16 dense matrix", MemoryTBps: 6.0, Source: "AMD Instinct MI325X data sheet"}},
	{regexp.MustCompile(`(?i)mi300x`), gpuprofile.Peak{ComputeTFLOPS: 1307.4, Precision: "BF16 dense matrix", MemoryTBps: 5.3, Source: "AMD Instinct MI300X data sheet"}},
}

// LookupPeak returns the datasheet peak of an accelerator name, if listed.
func LookupPeak(accelerator string) (gpuprofile.Peak, bool) {
	name := strings.TrimSpace(accelerator)
	for _, entry := range peaks {
		if entry.pattern.MatchString(name) {
			return entry.peak, true
		}
	}
	return gpuprofile.Peak{}, false
}

// roofline places each recorded 16-bit matrix-multiply shape at its
// arithmetic intensity (FLOPs per byte moved) and achieved throughput, and
// says which side of the ridge point most matrix-multiply time falls on.
// Shapes come from the profiler's record_shapes; under CUDA graphs the
// operators are not recorded, so graph-replayed kernels are not covered.
func (p *prepared) roofline(options Options) gpuprofile.Roofline {
	result := gpuprofile.Roofline{Position: "unknown", Points: []gpuprofile.RooflinePoint{}}
	accelerator := options.Accelerator
	if accelerator == "" {
		accelerator = p.trace.Accelerator()
	}
	if accelerator != "" {
		result.Accelerator = pointer(printable(accelerator, 128))
	}
	peak, known := gpuprofile.Peak{}, false
	if options.Peak != nil {
		peak, known = *options.Peak, true
	} else if accelerator != "" {
		peak, known = LookupPeak(accelerator)
	}
	if known {
		result.Peak = pointer(peak)
		result.RidgeFLOPPerByte = pointer(round(peak.ComputeTFLOPS*1e12/(peak.MemoryTBps*1e12), 1))
	}
	type group struct {
		flops, bytes float64
		kernelNS     int64
	}
	groups := map[int32]*group{}
	countedOp := map[int]bool{}
	var gemmNS, coveredNS int64
	for _, kernel := range p.trace.Kernels {
		category := p.category(kernel)
		length := kernel.End - kernel.Start
		if category == gpuprofile.CategoryGEMM {
			gemmNS += length
		}
		_, launch, ok := p.launchOfKernel(kernel)
		if !ok {
			continue
		}
		opIndex := p.innermostOp(launch)
		if opIndex < 0 {
			continue
		}
		op := p.trace.Ops[opIndex]
		if op.ShapeLabel < 0 || !sixteenBit(p.trace.Names[op.ShapeLabel]) {
			continue
		}
		if category == gpuprofile.CategoryGEMM {
			coveredNS += length
		}
		entry := groups[op.ShapeLabel]
		if entry == nil {
			entry = &group{}
			groups[op.ShapeLabel] = entry
		}
		entry.kernelNS += length
		if !countedOp[opIndex] {
			countedOp[opIndex] = true
			entry.flops += op.FLOPs
			entry.bytes += op.Bytes
		}
	}
	if gemmNS > 0 {
		result.CoveredShare = pointer(shareOf(coveredNS, gemmNS))
	}
	labels := make([]int32, 0, len(groups))
	for label, entry := range groups {
		if entry.kernelNS > 0 && entry.flops > 0 && entry.bytes > 0 {
			labels = append(labels, label)
		}
	}
	sort.Slice(labels, func(i, j int) bool {
		left, right := groups[labels[i]].kernelNS, groups[labels[j]].kernelNS
		if left != right {
			return left > right
		}
		return p.trace.Names[labels[i]] < p.trace.Names[labels[j]]
	})
	var boundNS, memoryNS int64
	for _, label := range labels {
		entry := groups[label]
		intensity := entry.flops / entry.bytes
		achieved := entry.flops / (float64(entry.kernelNS) / 1e9) / 1e12
		bound := "compute"
		if result.RidgeFLOPPerByte != nil && intensity < *result.RidgeFLOPPerByte {
			bound = "memory"
			memoryNS += entry.kernelNS
		}
		boundNS += entry.kernelNS
		if len(result.Points) < gpuprofile.MaxRooflinePoints {
			result.Points = append(result.Points, gpuprofile.RooflinePoint{
				Label: printable(p.trace.Names[label], 200), Category: gpuprofile.CategoryGEMM, FLOPs: round(entry.flops, 0), Bytes: round(entry.bytes, 0),
				KernelMS: ms(entry.kernelNS), Intensity: round(intensity, 2), AchievedTFLOPS: round(achieved, 3), Bound: bound,
			})
		}
	}
	// A rounded value of zero would fail validation as a measurement.
	kept := result.Points[:0]
	for _, point := range result.Points {
		if point.FLOPs > 0 && point.Bytes > 0 && point.KernelMS > 0 && point.Intensity > 0 && point.AchievedTFLOPS > 0 {
			kept = append(kept, point)
		}
	}
	result.Points = kept
	switch {
	case !known && accelerator == "":
		result.Reason = "The trace does not name the accelerator, so its peak compute and memory bandwidth are unknown."
	case !known:
		result.Reason = fmt.Sprintf("No datasheet peak is listed for %s; pass --peak-tflops and --peak-tbps.", printable(accelerator, 80))
	case gemmNS == 0:
		result.Reason = "No matrix-multiply kernels ran in the capture."
	case boundNS == 0:
		result.Reason = "No 16-bit matrix multiply had recorded shapes. Enable record_shapes; kernels replayed from CUDA graphs never have them."
	default:
		result.MemoryBoundShare = pointer(shareOf(memoryNS, boundNS))
		if coverage := *result.CoveredShare; coverage < minRooflineCoverage {
			result.Reason = fmt.Sprintf("Only %s of matrix-multiply time had recorded shapes, too little to place the workload.", percent(coverage))
		} else if *result.MemoryBoundShare >= 0.5 {
			result.Position = "memory_bound"
		} else {
			result.Position = "compute_bound"
		}
	}
	return result
}

func sixteenBit(label string) bool {
	return strings.HasSuffix(label, " bf16") || strings.HasSuffix(label, " fp16")
}
