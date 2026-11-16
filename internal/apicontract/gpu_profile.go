package apicontract

import "github.com/infercrane/infercrane/internal/gpuprofile"

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
		"schema_version":     map[string]any{"type": "string", "const": gpuprofile.SchemaVersion},
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
	return map[string]any{
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
}
