package apicontract

import "github.com/infercrane/infercrane/internal/acceleratorlab"

func acceleratorLabRunQueryParameters() []map[string]any {
	return []map[string]any{
		{"name": "campaign_id", "in": "query", "required": false, "description": "Exact campaign ID. Omit to list runs from every campaign.", "schema": map[string]any{"type": "string", "maxLength": 256}},
		{"name": "limit", "in": "query", "required": false, "description": "Maximum runs returned, newest first.", "schema": map[string]any{"type": "integer", "minimum": 1, "maximum": 100, "default": 20}},
	}
}

// acceleratorLabRunSchemas documents the typed Accelerator Lab run read. The
// read copies stored values and never fills in a measurement. It carries
// digests, never artifact URIs or sizes, literature, credentials, tenant IDs,
// lease owners, or idempotency keys.
func acceleratorLabRunSchemas() map[string]any {
	text, boolean, integer, number := map[string]any{"type": "string"}, map[string]any{"type": "boolean"}, map[string]any{"type": "integer"}, map[string]any{"type": "number"}
	timestamp := map[string]any{"type": "string", "format": "date-time"}
	texts := map[string]any{"type": "array", "items": text}
	object := func(required []string, properties map[string]any) map[string]any {
		return map[string]any{"type": "object", "additionalProperties": false, "required": required, "properties": properties}
	}
	nullable := func(schema map[string]any) map[string]any {
		return map[string]any{"oneOf": []any{schema, map[string]any{"type": "null"}}}
	}
	hardware := object([]string{"vendor", "accelerator", "compute_capability"}, map[string]any{"vendor": text, "accelerator": text, "compute_capability": text})
	hotspot := object([]string{"id", "name", "family", "device_time_fraction", "mean_duration_us", "invocations", "shapes", "dtypes"}, map[string]any{
		"id": text, "name": text, "family": text, "device_time_fraction": number,
		"mean_duration_us": map[string]any{"type": []string{"number", "null"}, "description": "Null when the profiler did not record it."},
		"invocations":      map[string]any{"type": []string{"integer", "null"}, "description": "Null when the profiler did not record it."},
		"shapes":           texts, "dtypes": texts,
	})
	build := object([]string{"builder", "builder_version", "source_digest", "status", "checks", "receipt_digest", "artifact_digests"}, map[string]any{
		"builder": text, "builder_version": text, "source_digest": text, "status": text, "checks": texts, "receipt_digest": text,
		"artifact_digests": map[string]any{"type": "array", "items": text, "description": "Content digests of the built artifacts. Their storage locations are not exposed."},
	})
	correctness := object([]string{"smoke", "shape_sweep", "numerical_stability", "determinism", "edge_cases", "sanitizer"}, map[string]any{
		"smoke": boolean, "shape_sweep": boolean, "numerical_stability": boolean, "determinism": boolean, "edge_cases": boolean, "sanitizer": boolean,
	})
	attempt := object([]string{"iteration", "source_revision", "backend", "correctness", "baseline_median_us", "candidate_median_us", "kernel_speedup", "end_to_end_speedup", "decision", "reason", "completed_at"}, map[string]any{
		"iteration": integer, "source_revision": text, "backend": text, "correctness": correctness, "baseline_median_us": number, "candidate_median_us": number,
		"kernel_speedup": number, "end_to_end_speedup": number, "decision": text, "reason": text, "completed_at": timestamp,
	})
	return map[string]any{
		"AcceleratorLabRunList": map[string]any{
			"type": "object", "additionalProperties": false, "required": []string{"data", "boundary"},
			"properties": map[string]any{"data": map[string]any{"type": "array", "items": ref("AcceleratorLabRun")}, "boundary": text},
		},
		"AcceleratorLabRun": object([]string{"id", "campaign_id", "candidate_id", "status", "progress", "message", "error_code", "created_at", "updated_at", "completed_at", "target", "policy", "result_state", "result"}, map[string]any{
			"id": map[string]any{"type": "string", "description": "Durable operation ID."}, "campaign_id": text,
			"candidate_id": map[string]any{"type": "string", "description": "Empty when the stored request cannot be read."},
			"status":       map[string]any{"type": "string", "description": "Durable operation status as stored."},
			"progress":     map[string]any{"type": "integer", "minimum": 0, "maximum": 100}, "message": text,
			"error_code": map[string]any{"type": []string{"string", "null"}}, "created_at": timestamp, "updated_at": timestamp,
			"completed_at": map[string]any{"type": []string{"string", "null"}, "format": "date-time"},
			"target":       nullable(ref("AcceleratorLabRunTarget")), "policy": nullable(ref("AcceleratorLabRunPolicy")),
			"result_state": map[string]any{"type": "string", "enum": []string{"pending", "recorded", "unreadable"}, "description": "pending: the worker has not recorded a result. unreadable: the stored result does not decode or has another schema version."},
			"result":       nullable(ref("AcceleratorLabRunResult")),
		}),
		"AcceleratorLabRunTarget": object([]string{"model", "runtime", "hardware", "topology", "workload"}, map[string]any{
			"model":    object([]string{"repository", "revision"}, map[string]any{"repository": text, "revision": text}),
			"runtime":  object([]string{"name", "version", "image_digest"}, map[string]any{"name": text, "version": text, "image_digest": text}),
			"hardware": hardware,
			"topology": object([]string{"mode", "nodes", "accelerators", "tensor_parallel", "pipeline_parallel", "expert_parallel"}, map[string]any{
				"mode": text, "nodes": integer, "accelerators": integer, "tensor_parallel": integer, "pipeline_parallel": integer, "expert_parallel": integer,
				"prefill_replicas": integer, "decode_replicas": integer,
			}),
			"workload": object([]string{"digest", "modality", "phase", "batch_size", "concurrency", "input_tokens", "output_tokens"}, map[string]any{
				"digest": text, "modality": text, "phase": text, "batch_size": integer, "concurrency": integer, "input_tokens": integer, "output_tokens": integer,
			}),
		}),
		"AcceleratorLabRunPolicy": object([]string{"max_cost_usd", "min_hotspot_fraction", "min_amdahl_ceiling", "target_end_to_end_speedup", "min_kernel_speedup", "max_memory_regression_percent", "require_hidden_shapes", "require_sanitizer", "require_serving_replay", "require_quality", "allow_generated_kernels"}, map[string]any{
			"max_cost_usd": number, "min_hotspot_fraction": number, "min_amdahl_ceiling": number, "target_end_to_end_speedup": number,
			"min_kernel_speedup": number, "max_memory_regression_percent": number, "require_hidden_shapes": boolean, "require_sanitizer": boolean,
			"require_serving_replay": boolean, "require_quality": boolean, "allow_generated_kernels": boolean,
		}),
		"AcceleratorLabRunResult": object([]string{"schema_version", "input_digest", "state", "evidence_state", "winner_id", "cost_usd", "profile", "plan", "experiments", "limitations"}, map[string]any{
			"schema_version": map[string]any{"type": "string", "const": acceleratorlab.ResultSchemaV1}, "input_digest": text, "state": text, "evidence_state": text,
			"winner_id": map[string]any{"type": []string{"string", "null"}}, "cost_usd": number,
			"profile": object([]string{"tool", "tool_version", "captured_at", "runtime_image_digest", "artifact_digest", "hotspots"}, map[string]any{
				"tool": text, "tool_version": text, "captured_at": timestamp, "runtime_image_digest": text, "artifact_digest": text,
				"hotspots": map[string]any{"type": "array", "items": hotspot},
			}),
			"plan": object([]string{"algorithm_version", "registry_version", "selection_boundary", "candidates", "rejected"}, map[string]any{
				"algorithm_version": text, "registry_version": text, "selection_boundary": text,
				"candidates": map[string]any{"type": "array", "items": map[string]any{"type": "object", "additionalProperties": true, "description": "The kernel planner's candidate as recorded."}},
				"rejected":   map[string]any{"type": "array", "items": object([]string{"hotspot_id", "reason"}, map[string]any{"hotspot_id": text, "reason": text})},
			}),
			"experiments": map[string]any{"type": "array", "items": ref("AcceleratorLabRunExperiment")},
			"limitations": texts,
		}),
		"AcceleratorLabRunExperiment": object([]string{"candidate_id", "operator_family", "state", "failure_code", "reasons", "source", "generated", "build", "qualification", "research"}, map[string]any{
			"candidate_id": text, "operator_family": text, "state": text, "failure_code": map[string]any{"type": []string{"string", "null"}},
			"reasons":   map[string]any{"type": "array", "items": text, "description": "URIs inside a reason are replaced with [uri redacted]."},
			"source":    object([]string{"implementation_id", "revision", "license", "backend"}, map[string]any{"implementation_id": text, "revision": text, "license": text, "backend": text}),
			"generated": map[string]any{"type": "boolean", "description": "True when the experiment's source was generated rather than supplied."},
			"build":     nullable(build), "qualification": nullable(ref("AcceleratorLabRunQualification")), "research": nullable(ref("AcceleratorLabRunResearch")),
		}),
		"AcceleratorLabRunQualification": object([]string{"correctness_passed", "hidden_shapes_passed", "sanitizer_passed", "quality_passed", "serving_replay_measured", "kernel_speedup", "end_to_end_speedup", "memory_regression_percent", "cost_regression_percent", "build_artifact_digest", "profiler_artifact_digest", "benchmark_evidence_id", "quality_evidence_id", "measured_at", "hardware", "runtime_image_digest", "workload_digest", "cost_usd"}, map[string]any{
			"correctness_passed": boolean, "hidden_shapes_passed": boolean, "sanitizer_passed": boolean, "quality_passed": boolean, "serving_replay_measured": boolean,
			"kernel_speedup": number, "end_to_end_speedup": number, "memory_regression_percent": number, "cost_regression_percent": number,
			"build_artifact_digest": text, "profiler_artifact_digest": text, "benchmark_evidence_id": text, "quality_evidence_id": text,
			"measured_at": timestamp, "hardware": hardware, "runtime_image_digest": text, "workload_digest": text, "cost_usd": number,
		}),
		"AcceleratorLabRunResearch": object([]string{"state", "best_source_revision", "best_kernel_speedup", "best_end_to_end_speedup", "receipt_digest", "attempts"}, map[string]any{
			"state": text, "best_source_revision": text, "best_kernel_speedup": number, "best_end_to_end_speedup": number, "receipt_digest": text,
			"attempts": map[string]any{"type": "array", "items": attempt},
		}),
	}
}
