package apicontract

import (
	"sort"

	"github.com/infercrane/infercrane/internal/gpuprofile"
)

// gpuProfileSchemas describes the recording a collector sends and the profile
// an endpoint read returns. Bounds come from internal/gpuprofile.
func gpuProfileSchemas() map[string]any {
	timestamp := map[string]any{"type": "string", "format": "date-time"}
	captureMetadata := map[string]any{
		"tool":             map[string]any{"type": "string", "pattern": "^[a-z][a-z0-9._-]{0,63}$", "description": "Capture tool, for example cupti-pc-sampling."},
		"tool_version":     map[string]any{"type": "string", "pattern": "^[A-Za-z0-9][A-Za-z0-9._+-]{0,63}$"},
		"evidence_class":   map[string]any{"type": "string", "const": "measured"},
		"accelerator":      map[string]any{"type": []string{"string", "null"}, "minLength": 1, "maxLength": 128},
		"gpus":             map[string]any{"type": []string{"integer", "null"}, "minimum": 1, "maximum": gpuprofile.MaxGPUs},
		"started_at":       timestamp,
		"ended_at":         timestamp,
		"content_recorded": map[string]any{"type": "boolean", "const": false},
	}
	captureRequired := []string{"tool", "tool_version", "evidence_class", "accelerator", "gpus", "started_at", "ended_at", "content_recorded"}
	captureWithID := map[string]any{"id": map[string]any{"type": "string", "pattern": "^gpc-[0-9a-f]{32}$"}}
	for key, value := range captureMetadata {
		captureWithID[key] = value
	}
	body := map[string]any{
		"schema_version":     map[string]any{"type": "string", "enum": []string{gpuprofile.SchemaVersion, gpuprofile.SchemaVersionV2}, "description": "v1 carries call stacks only; v2 adds the trace analysis and an optional timeline excerpt."},
		"analysis":           map[string]any{"$ref": "#/components/schemas/GPUProfileAnalysis", "description": "Required in v2, absent in v1."},
		"timeline_excerpt":   map[string]any{"$ref": "#/components/schemas/GPUTimelineExcerpt", "description": "v2 only: the busiest part of the capture for a timeline viewer."},
		"timeline":           ref("GPUProfileTimeline"),
		"frames":             map[string]any{"type": "array", "minItems": 1, "maxItems": gpuprofile.MaxFrames, "items": ref("GPUProfileFrame")},
		"stacks":             map[string]any{"type": "array", "minItems": 1, "maxItems": gpuprofile.MaxStacks, "items": ref("GPUProfileStack")},
		"unavailable_layers": map[string]any{"type": "array", "uniqueItems": true, "maxItems": len(gpuprofile.Kinds), "items": map[string]any{"type": "string", "enum": gpuprofile.Kinds}, "description": "Layers this capture could not record, for example sass and stall without PC sampling."},
	}
	recording := map[string]any{"capture": map[string]any{"type": "object", "additionalProperties": false, "required": captureRequired, "properties": captureMetadata}}
	profile := map[string]any{
		"endpoint":    map[string]any{"type": "string"},
		"deployment":  map[string]any{"type": "string", "description": "The deployment whose active revision was profiled."},
		"revision":    map[string]any{"type": "integer", "minimum": 1},
		"capture":     map[string]any{"type": "object", "additionalProperties": false, "required": append([]string{"id"}, captureRequired...), "properties": captureWithID},
		"recorded_at": timestamp,
	}
	for key, value := range body {
		recording[key], profile[key] = value, value
	}
	result := map[string]any{
		"GPUProfileRecording": map[string]any{
			"type": "object", "additionalProperties": false,
			"description": "One content-free GPU profile capture: sampled GPU time attributed to root-first stacks of interned frames.",
			"required":    []string{"schema_version", "capture", "timeline", "frames", "stacks", "unavailable_layers"},
			"properties":  recording,
		},
		"GPUProfile": map[string]any{
			"type": "object", "additionalProperties": false,
			"required":   []string{"schema_version", "endpoint", "deployment", "revision", "capture", "timeline", "frames", "stacks", "unavailable_layers", "recorded_at"},
			"properties": profile,
		},
		"GPUProfileReceipt": map[string]any{
			"type": "object", "additionalProperties": false,
			"required": []string{"id", "deployment", "revision", "started_at", "ended_at", "frames", "stacks", "recorded_at"},
			"properties": map[string]any{
				"id": map[string]any{"type": "string", "pattern": "^gpc-[0-9a-f]{32}$"}, "deployment": map[string]any{"type": "string"},
				"revision": map[string]any{"type": "integer", "minimum": 1}, "started_at": timestamp, "ended_at": timestamp,
				"frames": map[string]any{"type": "integer", "minimum": 1}, "stacks": map[string]any{"type": "integer", "minimum": 1}, "recorded_at": timestamp,
			},
		},
		"GPUProfileTimeline": map[string]any{
			"type": "object", "additionalProperties": false, "required": []string{"bucket_seconds", "buckets"},
			"properties": map[string]any{
				"bucket_seconds": map[string]any{"type": "integer", "minimum": 1, "maximum": 3600},
				"buckets": map[string]any{"type": "array", "maxItems": gpuprofile.MaxBuckets, "items": map[string]any{
					"type": "object", "additionalProperties": false, "required": []string{"start", "gpu_seconds"},
					"properties": map[string]any{"start": timestamp, "gpu_seconds": map[string]any{"type": []string{"number", "null"}, "minimum": 0, "description": "Null when the bucket has no samples, never zero."}},
				}},
			},
		},
		"GPUProfileFrame": map[string]any{
			"type": "object", "additionalProperties": false, "required": []string{"kind", "name"},
			"properties": map[string]any{
				"kind": map[string]any{"type": "string", "enum": gpuprofile.Kinds},
				"name": map[string]any{"type": "string", "minLength": 1, "maxLength": gpuprofile.MaxFrameName},
				"file": map[string]any{"type": "string", "maxLength": gpuprofile.MaxFrameFile},
				"line": map[string]any{"type": "integer", "minimum": 1},
			},
		},
		"GPUProfileStack": map[string]any{
			"type": "object", "additionalProperties": false, "required": []string{"frames", "gpu_ns"},
			"properties": map[string]any{
				"frames": map[string]any{"type": "array", "minItems": 1, "maxItems": gpuprofile.MaxStackDepth, "items": map[string]any{"type": "integer", "minimum": 0}, "description": "Root-first indexes into frames."},
				"gpu_ns": map[string]any{"type": "integer", "minimum": 1, "description": "GPU time attributed to exactly this stack."},
			},
		},
	}
	for name, schema := range gpuAnalysisSchemas() {
		result[name] = schema
	}
	return result
}

func object(required []string, properties map[string]any) map[string]any {
	return map[string]any{"type": "object", "additionalProperties": false, "required": required, "properties": properties}
}

func keys(properties map[string]any) []string {
	names := make([]string, 0, len(properties))
	for name := range properties {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// closed is an object whose every property is required.
func closed(properties map[string]any) map[string]any { return object(keys(properties), properties) }

// gpuAnalysisSchemas describes gpu-trace-analysis-v1 and the reads built on
// it. A nullable number is unknown, never zero, and its section's reason
// says why.
func gpuAnalysisSchemas() map[string]any {
	share := map[string]any{"type": "number", "minimum": 0, "maximum": 1}
	nullableShare := map[string]any{"type": []string{"number", "null"}, "minimum": 0, "maximum": 1}
	number := map[string]any{"type": "number", "minimum": 0}
	nullableNumber := map[string]any{"type": []string{"number", "null"}, "minimum": 0}
	integer := map[string]any{"type": "integer", "minimum": 0}
	text := map[string]any{"type": "string"}
	nullableText := map[string]any{"type": []string{"string", "null"}}
	timestamp := map[string]any{"type": "string", "format": "date-time"}
	category := map[string]any{"type": "string", "enum": gpuprofile.Categories}
	phase := map[string]any{"type": "string", "enum": []string{gpuprofile.PhasePrefill, gpuprofile.PhaseDecode, gpuprofile.PhaseMixed, gpuprofile.PhaseUnknown}}
	list := func(items any, limit int) map[string]any {
		return map[string]any{"type": "array", "maxItems": limit, "items": items}
	}
	phaseTime := closed(map[string]any{"gpu_ms": number, "share": share, "steps": integer})
	verdict := closed(map[string]any{
		"bottleneck": map[string]any{"type": "string", "enum": []string{gpuprofile.BottleneckHostLaunch, gpuprofile.BottleneckCommunication, gpuprofile.BottleneckMemoryBandwidth, gpuprofile.BottleneckCompute, gpuprofile.BottleneckAttention, gpuprofile.BottleneckBusy, gpuprofile.BottleneckUnknown}},
		"sentence":   map[string]any{"type": "string", "minLength": 1, "maxLength": 280},
		"evidence":   map[string]any{"type": "string", "maxLength": 400, "description": "The measured values the sentence rests on."},
	})
	analysis := closed(map[string]any{
		"algorithm_version": map[string]any{"type": "string", "const": gpuprofile.AnalysisAlgorithm},
		"trace": closed(map[string]any{
			"format": map[string]any{"type": "string", "enum": []string{"kineto", "rocprofv3", "nsys"}}, "engine": map[string]any{"type": []string{"string", "null"}, "enum": []any{"vllm", "sglang", nil}},
			"engine_version": nullableText, "devices": map[string]any{"type": "integer", "minimum": 1}, "streams": map[string]any{"type": "integer", "minimum": 1},
			"kernels": map[string]any{"type": "integer", "minimum": 1}, "cpu_ops_recorded": map[string]any{"type": "boolean"}, "shapes_recorded": map[string]any{"type": "boolean"},
			"launches_recorded": map[string]any{"type": "boolean"}, "window_ms": number, "truncated": map[string]any{"type": "boolean", "description": "The trace held more events than one analysis reads."},
		}),
		"verdict": verdict,
		"utilization": closed(map[string]any{
			"busy_share": share, "compute_share": share, "non_compute_share": share, "idle_share": share, "bucket_ms": number,
			"busy_series": list(share, gpuprofile.MaxSeries),
		}),
		"idle": closed(map[string]any{
			"idle_ms": number, "host_wait_share": nullableShare, "launch_gap_share": nullableShare, "other_share": nullableShare, "reason": text,
			"bubbles": closed(map[string]any{
				"count": integer, "p50_us": nullableNumber, "p95_us": nullableNumber, "max_us": nullableNumber,
				"top": list(closed(map[string]any{"start_ms": number, "duration_us": number, "device": integer, "cause": map[string]any{"type": "string", "enum": []string{"host_wait", "launch_gap", "other"}}}), gpuprofile.MaxBubbles),
			}),
		}),
		"launch": closed(map[string]any{
			"launches": integer, "graph_launches": integer, "per_second": number, "host_launch_share": nullableShare,
			"launch_delay_p50_us": nullableNumber, "launch_delay_p95_us": nullableNumber, "short_kernel_share": nullableShare, "reason": text,
		}),
		"categories":  list(closed(map[string]any{"category": category, "kernel_ms": number, "share": share, "count": integer}), len(gpuprofile.Categories)),
		"top_kernels": list(closed(map[string]any{"name": text, "category": category, "phase": map[string]any{"type": []string{"string", "null"}}, "count": integer, "total_ms": number, "mean_us": number, "max_us": number, "share": share}), gpuprofile.MaxTopKernels),
		"overlap":     closed(map[string]any{"communication_ms": number, "overlapped_share": nullableShare, "reason": text}),
		"phases": closed(map[string]any{
			"source":  map[string]any{"type": "string", "enum": []string{"annotations", "kernel_names", "unknown"}},
			"prefill": phaseTime, "decode": phaseTime, "mixed": phaseTime, "unknown": phaseTime,
		}),
		"steps": closed(map[string]any{
			"source": map[string]any{"type": "string", "enum": []string{"annotations", "idle_gaps", "none"}}, "marker": nullableText, "count": integer,
			"p50_ms": nullableNumber, "p95_ms": nullableNumber, "reason": text,
			"items": list(closed(map[string]any{"start_ms": number, "duration_ms": number, "gpu_busy_ms": number, "kernels": integer, "phase": phase}), gpuprofile.MaxStepItems),
		}),
		"roofline": closed(map[string]any{
			"accelerator": nullableText,
			"peak": map[string]any{"oneOf": []any{map[string]any{"type": "null"}, closed(map[string]any{
				"compute_tflops": number, "precision": text, "memory_tbps": number, "source": map[string]any{"type": "string", "description": "Vendor datasheet the peak comes from, or operator."},
			})}},
			"ridge_flop_per_byte": nullableNumber, "position": map[string]any{"type": "string", "enum": []string{"memory_bound", "compute_bound", "unknown"}},
			"memory_bound_share": nullableShare, "covered_share": nullableShare, "reason": text,
			"points": list(closed(map[string]any{"label": text, "category": category, "flops": number, "bytes": number, "kernel_ms": number, "intensity": number, "achieved_tflops": number, "bound": map[string]any{"type": "string", "enum": []string{"memory", "compute"}}}), gpuprofile.MaxRooflinePoints),
		}),
		"hardware": map[string]any{"oneOf": []any{map[string]any{"type": "null"}, closed(map[string]any{
			"source": map[string]any{"type": "string", "const": "dcgm-exporter"}, "interval_ms": number,
			"series": list(closed(map[string]any{"field": text, "unit": text, "values": list(nullableNumber, gpuprofile.MaxSeries)}), gpuprofile.MaxHardwareSeries),
		})}},
		"findings": list(closed(map[string]any{"rule": text, "observed": map[string]any{"type": "number"}, "comparison": map[string]any{"type": "string", "enum": []string{">=", "<=", ">", "<"}}, "threshold": map[string]any{"type": "number"}, "unit": text, "sentence": text}), gpuprofile.MaxFindings),
	})
	analysis["description"] = "gpu-trace-analysis-v1: where GPU time goes in one capture. Shares are 0..1; null is unknown and the section's reason says why."
	excerpt := closed(map[string]any{
		"start_ms": number, "duration_ms": map[string]any{"type": "number", "minimum": 0, "maximum": gpuprofile.MaxExcerptMS},
		"names": list(text, gpuprofile.MaxExcerptNames), "categories": list(map[string]any{"type": "string", "enum": append(append([]string{}, gpuprofile.Categories...), gpuprofile.CategoryLaunch)}, gpuprofile.MaxExcerptNames),
		"lanes":     list(closed(map[string]any{"kind": map[string]any{"type": "string", "enum": []string{"gpu", "host"}}, "device": map[string]any{"type": []string{"integer", "null"}}, "stream": map[string]any{"type": []string{"integer", "null"}}, "label": text}), gpuprofile.MaxExcerptLanes),
		"events":    map[string]any{"type": "array", "maxItems": gpuprofile.MaxExcerptEvents, "description": "[lane, name, start_us, duration_us], start relative to the excerpt.", "items": map[string]any{"type": "array", "minItems": 4, "maxItems": 4, "items": number}},
		"steps":     list(closed(map[string]any{"start_us": number, "duration_us": number, "phase": phase}), gpuprofile.MaxExcerptSteps),
		"truncated": map[string]any{"type": "boolean"},
	})
	summary := closed(map[string]any{
		"id": map[string]any{"type": "string", "pattern": "^gpc-[0-9a-f]{32}$"}, "deployment": text, "revision": map[string]any{"type": "integer", "minimum": 1},
		"active":         map[string]any{"type": "boolean", "description": "The capture belongs to the deployment's serving revision."},
		"schema_version": text, "tool": text, "tool_version": text, "accelerator": nullableText, "gpus": map[string]any{"type": []string{"integer", "null"}},
		"started_at": timestamp, "ended_at": timestamp, "recorded_at": timestamp,
		"verdict":    map[string]any{"oneOf": []any{map[string]any{"type": "null"}, verdict}},
		"busy_share": nullableShare,
	})
	metric := closed(map[string]any{
		"metric": text, "label": text, "unit": text, "base": map[string]any{"type": []string{"number", "null"}}, "head": map[string]any{"type": []string{"number", "null"}},
		"delta": map[string]any{"type": []string{"number", "null"}}, "better": map[string]any{"type": "string", "enum": []string{"higher", "lower", ""}},
		"improved": map[string]any{"type": []string{"boolean", "null"}, "description": "Null when either side is unknown or the change is within two points or two percent."},
	})
	return map[string]any{
		"GPUProfileAnalysis":    analysis,
		"GPUTimelineExcerpt":    excerpt,
		"GPUProfileSummary":     summary,
		"GPUProfileSummaryList": object([]string{"data"}, map[string]any{"data": map[string]any{"type": "array", "items": ref("GPUProfileSummary")}}),
		"GPUProfileComparison": closed(map[string]any{
			"base": ref("GPUProfileSummary"), "head": ref("GPUProfileSummary"), "warnings": list(text, 8),
			"metrics":    list(metric, 32),
			"categories": list(closed(map[string]any{"category": category, "base_ms_per_s": number, "head_ms_per_s": number, "base_share": share, "head_share": share}), len(gpuprofile.Categories)),
			"kernels":    list(closed(map[string]any{"name": text, "category": category, "base_ms_per_s": nullableNumber, "head_ms_per_s": nullableNumber}), gpuprofile.MaxCompareKernels),
		}),
	}
}
