package apicontract

// Schemas of the results store's routes (internal/resultstore,
// internal/resultprior). The record mirrors schemas/result-record-v1.schema.json
// and its result schemas/technique-result-v1.schema.json.

func resultRecordSchemas() map[string]any {
	text, number := map[string]any{"type": "string"}, map[string]any{"type": "number"}
	integer := map[string]any{"type": "integer", "minimum": 0}
	texts := map[string]any{"type": "array", "items": text}
	object := func(required []string, properties map[string]any) map[string]any {
		return map[string]any{"type": "object", "additionalProperties": false, "required": required, "properties": properties}
	}
	model := object([]string{"architecture"}, map[string]any{
		"id": text, "revision": text, "architecture": map[string]any{"type": "string", "enum": []string{"dense", "moe", "hybrid", "unknown"}},
		"params_b": number, "active_params_b": number, "checkpoint": text, "features": texts})
	result := object([]string{"schema_version", "technique", "technique_version", "candidate", "hardware", "software", "workload", "precision", "baseline", "correctness", "measured", "reproduction"}, map[string]any{
		"schema_version": map[string]any{"type": "string", "enum": []string{"infercrane.dev/technique-result/v1"}}, "technique": text, "technique_version": text, "candidate": text,
		"hardware":          object([]string{"accelerator", "count"}, map[string]any{"accelerator": text, "count": integer, "driver": text}),
		"software":          object([]string{"engine", "engine_version", "image_digest"}, map[string]any{"engine": text, "engine_version": text, "image_digest": text}),
		"workload":          object([]string{"shape_digest", "kind", "load", "requests"}, map[string]any{"shape_digest": text, "kind": text, "load": integer, "requests": integer}),
		"precision":         object([]string{"weights", "kv_cache"}, map[string]any{"weights": text, "kv_cache": text}),
		"baseline":          object([]string{"setup", "basis"}, map[string]any{"setup": text, "basis": text}),
		"correctness":       object([]string{"method", "verdict"}, map[string]any{"method": text, "verdict": text}),
		"measured":          object([]string{"metric", "change", "noise_floor", "decision"}, map[string]any{"metric": text, "change": number, "noise_floor": number, "decision": map[string]any{"type": "string", "enum": []string{"improved", "no_measurable_difference", "regressed", "unknown"}}}),
		"reproduction":      object([]string{"command", "commit", "image_digest", "traffic_shape"}, map[string]any{"command": text, "commit": text, "image_digest": text, "traffic_shape": text}),
		"required_evidence": texts,
		"evidence":          map[string]any{"type": "array", "items": object([]string{"name", "digest"}, map[string]any{"name": text, "digest": text, "summary": text})},
	})
	record := object([]string{"schema_version", "id", "sharing", "model", "setup", "cell", "observed", "result", "source", "recorded_at"}, map[string]any{
		"schema_version": map[string]any{"type": "string", "enum": []string{"infercrane.dev/result-record/v1"}},
		"id":             map[string]any{"type": "string", "description": "Derived from the record's content: the same record imported twice is stored once."},
		"sharing":        map[string]any{"type": "string", "enum": []string{"private", "numbers"}, "description": "private: read and used by its own organization only. numbers: rankings for other organizations may use its numbers, without names; it is never returned to them."},
		"model":          model,
		"setup": object([]string{"name", "speculation"}, map[string]any{"name": text, "label": text, "speculation": map[string]any{"type": "string", "enum": []string{"none", "mtp", "ngram", "draft-model", "eagle", "unknown"}},
			"drafts": integer, "quantization": text, "kv_cache": text, "flags": texts}),
		"cell":        object([]string{"id", "set", "key"}, map[string]any{"id": text, "set": text, "key": text}),
		"observed":    object([]string{"median", "min", "max", "runs"}, map[string]any{"median": number, "min": number, "max": number, "runs": integer}),
		"result":      result,
		"source":      object([]string{"kind", "importer"}, map[string]any{"kind": text, "file": text, "digest": text, "importer": text}),
		"recorded_at": map[string]any{"type": "string", "format": "date-time"},
		"judgement": map[string]any{"type": "object", "additionalProperties": false,
			"description": "How the claim was judged beyond the result's own fields. The claim is the technique contract's own (the result's claim with its manifest's evidence); a measured result shows it only when it is not a baseline, was judged against its technique's manifest, and no check of its run failed.",
			"properties":  map[string]any{"manifest": text, "failed": texts, "unsettled": map[string]any{"type": "array", "items": text, "description": "Checks the run could not decide (its unknown); never a failed one."}, "dry": map[string]any{"type": "boolean"}, "baseline": map[string]any{"type": "boolean", "description": "A setup measured to compare with; it needs no manifest and makes no claim."}}},
	})
	knobs := object([]string{"engine"}, map[string]any{"engine": text, "speculation": text, "drafts": integer, "kv_cache": text, "quantization": text, "flags": text})
	evidence := object([]string{"cell", "gain", "spread", "distance", "weight"}, map[string]any{"cell": text, "gain": number, "spread": number, "distance": number, "weight": number, "shared": map[string]any{"type": "boolean"}})
	return map[string]any{
		"ResultRecord":     record,
		"ResultRecordList": object([]string{"data"}, map[string]any{"data": map[string]any{"type": "array", "items": ref("ResultRecord")}}),
		"ResultRecordImport": map[string]any{"type": "object", "additionalProperties": false, "required": []string{"schema_version", "records"},
			"description": "Measured results, as `infercrane results export` writes them. Their sharing is this request's, never their own field's; importing again states the current choice.",
			"properties": map[string]any{"schema_version": map[string]any{"type": "string", "enum": []string{"infercrane.dev/result-records/v1"}},
				"sharing": map[string]any{"type": "string", "enum": []string{"private", "numbers"}}, "records": map[string]any{"type": "array", "minItems": 1, "maxItems": 2000, "items": ref("ResultRecord")}}},
		"ResultRecordImportReceipt": object([]string{"added", "received", "sharing"}, map[string]any{"added": integer, "received": integer, "sharing": text}),
		"ResultRankQuery": map[string]any{"type": "object", "additionalProperties": false, "required": []string{"model", "accelerator", "kind", "load", "metric", "current", "candidates"},
			"description": "The traffic kind, load, GPU and model to rank for, and the setups that could be measured there. Every gain is against current.",
			"properties": map[string]any{"model": model, "accelerator": text, "engine_version": text, "kind": text, "load": map[string]any{"type": "integer", "minimum": 1}, "metric": text, "current": knobs,
				"candidates": map[string]any{"type": "array", "minItems": 1, "maxItems": 32, "items": object([]string{"name", "knobs"}, map[string]any{"name": text, "knobs": knobs})}}},
		"ResultRanking": map[string]any{"type": "object", "additionalProperties": false, "required": []string{"algorithm", "metric", "records", "cells", "ranked"},
			"description": "Setups ordered by the lower bound of their estimated gain over the current setup in similar measured results. It orders what to measure; it is not a result. A setup nothing similar measured is last and unknown.",
			"properties": map[string]any{"algorithm": text, "metric": text, "records": integer, "cells": integer, "ranked": map[string]any{"type": "array", "items": object(
				[]string{"name", "setup", "estimate", "lower", "cells", "weight", "known", "reason"},
				map[string]any{"name": text, "setup": text, "estimate": number, "lower": number, "cells": integer, "weight": number, "known": map[string]any{"type": "boolean"},
					"evidence": map[string]any{"type": "array", "items": evidence}, "reason": text})}}},
	}
}

// resultRecordQueryParameters are the list route's filters.
func resultRecordQueryParameters() []map[string]any {
	text := func(name, description string) map[string]any {
		return map[string]any{"name": name, "in": "query", "required": false, "description": description, "schema": map[string]any{"type": "string", "maxLength": 128}}
	}
	whole := func(name, description string) map[string]any {
		return map[string]any{"name": name, "in": "query", "required": false, "description": description, "schema": map[string]any{"type": "integer", "minimum": 0}}
	}
	return []map[string]any{text("model", "Model ID."), text("accelerator", "GPU, for example H100."), text("engine", "Engine, for example vllm."), text("kind", "Traffic kind, for example short."),
		text("metric", "Metric, for example output_tokens_per_second."), text("technique", "Technique ID."), text("cell", "The ID of one measurement of one traffic kind at one load."),
		whole("min_load", "Fewest requests at once."), whole("max_load", "Most requests at once."), whole("limit", "At most this many results, newest first; 200 by default, 2,000 at most.")}
}
