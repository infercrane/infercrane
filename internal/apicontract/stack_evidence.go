package apicontract

import "github.com/infercrane/infercrane/internal/stackevidence"

func bestKnownStackQueryParameters() []map[string]any {
	return []map[string]any{
		{"name": "model", "in": "query", "required": true, "description": "Exact Hugging Face organization/repository, or a reviewed catalog name.", "schema": map[string]any{"type": "string", "minLength": 3, "maxLength": 192}},
		{"name": "goal", "in": "query", "required": false, "description": "What to rank by: lowest first-token p95, lowest estimated cost per 1M output tokens at the target, or most output tokens per second per GPU.", "schema": map[string]any{"type": "string", "enum": stackevidence.Goals, "default": stackevidence.GoalLowestCost}},
		{"name": "input_tokens", "in": "query", "required": false, "description": "Expected prompt length. Only stacks measured at the closest published workload shape are compared.", "schema": map[string]any{"type": "integer", "minimum": 1, "maximum": 1048576, "default": stackevidence.DefaultInputTokens}},
		{"name": "output_tokens", "in": "query", "required": false, "description": "Expected output length.", "schema": map[string]any{"type": "integer", "minimum": 1, "maximum": 262144, "default": stackevidence.DefaultOutputTokens}},
		{"name": "gpus", "in": "query", "required": false, "description": "Comma-separated accelerators you can use, for example H100,H200. Omit for any.", "schema": map[string]any{"type": "string", "maxLength": 1024}},
		{"name": "max_ttft_p95_ms", "in": "query", "required": false, "description": "Latency target: first-token p95 at most this many milliseconds. A point whose p95 was not published reads as unknown, never as met.", "schema": map[string]any{"type": "number", "minimum": 0, "maximum": 600000}},
		{"name": "min_per_user_output_tokens_per_second", "in": "query", "required": false, "description": "Latency target: each request streams at least this many output tokens per second.", "schema": map[string]any{"type": "number", "minimum": 0, "maximum": 10000}},
		{"name": "limit", "in": "query", "required": false, "description": "Maximum ranked stacks returned.", "schema": map[string]any{"type": "integer", "minimum": 1, "maximum": stackevidence.MaxLimit, "default": stackevidence.DefaultLimit}},
	}
}

// bestKnownStackSchemas describes GET /catalog/stacks and the stack a plan
// starts from. Every metric is nullable because sources publish different
// subsets; null always means "not published", never zero.
func bestKnownStackSchemas() map[string]any {
	str := map[string]any{"type": "string"}
	nullableNumber := map[string]any{"type": []string{"number", "null"}, "minimum": 0}
	date := map[string]any{"type": "string", "format": "date"}
	shape := map[string]any{
		"type": "object", "additionalProperties": false, "required": []string{"input_tokens", "output_tokens"},
		"properties": map[string]any{"input_tokens": map[string]any{"type": "integer", "minimum": 1}, "output_tokens": map[string]any{"type": "integer", "minimum": 1}, "stacks": map[string]any{"type": "integer", "minimum": 1}},
	}
	return map[string]any{
		"BestKnownStacks": map[string]any{
			"type": "object", "additionalProperties": false,
			"description": "Serving stacks ranked from cited benchmark evidence. Throughput and latency are copied from the source at one measured operating point; cost per 1M output tokens is an estimate from that throughput and a current price, or unknown.",
			"required":    []string{"schema_version", "dataset_version", "model", "goal", "ranked_by", "workload", "target", "stacks", "total", "excluded", "other_workloads", "evidence_policy"},
			"properties": map[string]any{
				"schema_version":  map[string]any{"type": "string", "const": stackevidence.ResultSchemaVersion},
				"dataset_version": str,
				"model": map[string]any{
					"type": "object", "additionalProperties": false, "required": []string{"requested", "match"},
					"properties": map[string]any{
						"requested": str,
						"match":     map[string]any{"type": "string", "enum": []string{stackevidence.MatchExact, stackevidence.MatchFamily, stackevidence.MatchNone}, "description": "family means no evidence exists for this exact checkpoint and rows are for a model with the same architecture, named on each row."},
						"family": map[string]any{
							"type": "object", "additionalProperties": false, "required": []string{"id", "label", "basis"},
							"properties": map[string]any{"id": str, "label": str, "basis": str},
						},
					},
				},
				"goal":      map[string]any{"type": "string", "enum": stackevidence.Goals},
				"ranked_by": map[string]any{"type": "string", "description": "One sentence naming the ranking metric and target."},
				"workload": map[string]any{
					"type": "object", "additionalProperties": false, "required": []string{"requested", "match"},
					"properties": map[string]any{
						"input_tokens": map[string]any{"type": "integer", "minimum": 1}, "output_tokens": map[string]any{"type": "integer", "minimum": 1},
						"requested": shape, "match": map[string]any{"type": "string", "enum": []string{stackevidence.MatchExact, stackevidence.MatchClosest, stackevidence.MatchNone}},
					},
				},
				"target": map[string]any{
					"type": "object", "additionalProperties": false,
					"properties": map[string]any{"max_ttft_p95_ms": map[string]any{"type": "number"}, "min_per_user_output_tokens_per_second": map[string]any{"type": "number"}},
				},
				"stacks":        map[string]any{"type": "array", "maxItems": stackevidence.MaxLimit, "items": ref("BestKnownStack")},
				"total":         map[string]any{"type": "integer", "minimum": 0, "description": "Ranked stacks before the limit."},
				"default_stack": map[string]any{"type": "string", "description": "Highest-ranked stack with a reviewed launch configuration; Deploy uses it unless another is chosen."},
				"excluded": map[string]any{"type": "array", "items": map[string]any{
					"type": "object", "additionalProperties": false, "required": []string{"id", "label", "reason"},
					"properties": map[string]any{"id": str, "label": str, "reason": str},
				}},
				"other_workloads": map[string]any{"type": "array", "items": shape},
				"evidence_policy": str,
			},
		},
		"BestKnownStack": map[string]any{
			"type": "object", "additionalProperties": false,
			"required": []string{"rank", "id", "label", "model", "model_match", "engine", "engine_version", "gpu", "gpu_vendor", "gpu_count", "precision", "parallelism", "techniques", "workload", "measured_at", "stale", "rank_basis", "operating_point", "metrics", "cost", "target", "source", "deploy", "limitations"},
			"properties": map[string]any{
				"rank": map[string]any{"type": "integer", "minimum": 1}, "id": str, "label": str,
				"model":       map[string]any{"type": "string", "description": "The checkpoint the source measured."},
				"model_match": map[string]any{"type": "string", "enum": []string{stackevidence.MatchExact, stackevidence.MatchFamily}},
				"engine":      str, "engine_version": str, "gpu": str, "gpu_vendor": map[string]any{"type": "string", "enum": []string{"nvidia", "amd"}},
				"gpu_count": map[string]any{"type": "integer", "minimum": 1}, "precision": str, "parallelism": str,
				"techniques": map[string]any{"type": "array", "items": str},
				"workload": map[string]any{
					"type": "object", "additionalProperties": false, "required": []string{"input_tokens", "output_tokens", "dataset"},
					"properties": map[string]any{"id": str, "input_tokens": map[string]any{"type": "integer"}, "output_tokens": map[string]any{"type": "integer"}, "dataset": str},
				},
				"measured_at": date,
				"stale":       map[string]any{"type": "boolean", "description": "Measured longer ago than the dataset's freshness window; stale rows rank after fresh ones."},
				"rank_basis":  map[string]any{"type": "string", "enum": []string{"cost_per_million_output_tokens", "ttft_p95", "per_user_output_tokens_per_second", "output_tokens_per_second_per_gpu"}},
				"operating_point": map[string]any{
					"type": "object", "additionalProperties": false, "required": []string{"concurrency", "ref"},
					"properties": map[string]any{"concurrency": map[string]any{"type": "integer", "minimum": 1}, "ref": str},
				},
				"metrics": map[string]any{
					"type": "object", "additionalProperties": false,
					"required": []string{"output_tokens_per_second", "output_tokens_per_second_per_gpu", "per_user_output_tokens_per_second", "ttft_p50_ms", "ttft_p95_ms", "ttft_p99_ms", "itl_p95_ms", "derived"},
					"properties": map[string]any{
						"output_tokens_per_second": map[string]any{"type": "number", "minimum": 0}, "output_tokens_per_second_per_gpu": map[string]any{"type": "number", "minimum": 0},
						"per_user_output_tokens_per_second": nullableNumber, "per_user_statistic": map[string]any{"type": "string", "enum": []string{"mean", "median"}},
						"ttft_p50_ms": nullableNumber, "ttft_p95_ms": nullableNumber, "ttft_p99_ms": nullableNumber, "itl_p95_ms": nullableNumber,
						"derived": map[string]any{"type": "array", "items": map[string]any{"type": "string", "enum": []string{"output_tokens_per_second", "output_tokens_per_second_per_gpu"}}, "description": "Throughputs the source did not publish itself: the published figure times or divided by the GPU count."},
					},
				},
				"cost": map[string]any{
					"type": "object", "additionalProperties": false, "required": []string{"state", "per_million_output_tokens_usd", "basis"},
					"properties": map[string]any{
						"state": map[string]any{"type": "string", "enum": []string{stackevidence.CostEstimated, stackevidence.CostUnknown}}, "per_million_output_tokens_usd": nullableNumber,
						"hourly_usd": map[string]any{"type": "number", "minimum": 0}, "provider": str, "region": str, "price_source": str, "price_authority": str,
						"price_observed_at": map[string]any{"type": "string", "format": "date-time"}, "basis": str,
					},
				},
				"target": map[string]any{"type": "string", "enum": []string{stackevidence.TargetMet, stackevidence.TargetUnknown, stackevidence.TargetNotGiven}},
				"source": map[string]any{
					"type": "object", "additionalProperties": false,
					"required": []string{"id", "kind", "label", "display", "publisher", "title", "url", "license", "license_url", "published_at", "ref"},
					"properties": map[string]any{
						"id": str, "kind": map[string]any{"type": "string", "enum": []string{stackevidence.SourcePublicBenchmark, stackevidence.SourceVendorRecipe, stackevidence.SourceMeasuredByInferCrane}},
						"label": str, "display": map[string]any{"type": "string", "description": "For example \"Public benchmark · SemiAnalysis InferenceMAX · 2025-10-09\" or \"Measured by InferCrane · 2026-09-23\"."},
						"publisher": str, "title": str, "url": map[string]any{"type": "string", "format": "uri"}, "license": str,
						"license_url": map[string]any{"type": "string", "format": "uri"}, "published_at": date, "ref": str,
					},
				},
				"deploy": map[string]any{
					"type": "object", "additionalProperties": false, "required": []string{"state"},
					"properties": map[string]any{
						"state":         map[string]any{"type": "string", "enum": []string{stackevidence.DeployReady, stackevidence.DeployNotReviewed}},
						"catalog_model": str, "profile": str, "reason": str,
					},
				},
				"limitations": map[string]any{"type": "array", "items": str},
			},
		},
		"IntentPlanStack": map[string]any{
			"type": "object", "additionalProperties": false, "required": []string{"id", "label", "source", "source_url", "measured_at"},
			"description": "The best-known stack this plan launches. Its cited numbers stay in GET /catalog/stacks.",
			"properties": map[string]any{
				"id": str, "label": str, "source": str, "source_url": map[string]any{"type": "string", "format": "uri"}, "measured_at": date,
			},
		},
	}
}
