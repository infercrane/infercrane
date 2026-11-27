package apicontract

// Schemas of the Proof Report (internal/proofreport): the before-and-after
// document of a proof engagement, read from finished runs and signed by
// content digest.

func proofReportSchemas() map[string]any {
	text := map[string]any{"type": "string"}
	texts := map[string]any{"type": "array", "items": text}
	maybe := map[string]any{"type": []any{"number", "null"}}
	maybeInt := map[string]any{"type": []any{"integer", "null"}}
	maybeBool := map[string]any{"type": []any{"boolean", "null"}}
	object := func(required []string, properties map[string]any) map[string]any {
		return map[string]any{"type": "object", "additionalProperties": false, "required": required, "properties": properties}
	}
	digest := map[string]any{"type": "string", "pattern": "^sha256:[0-9a-f]{64}$"}
	setup := map[string]any{"oneOf": []any{object([]string{"label", "model", "engine", "engine_version", "args", "accelerator", "accelerator_count", "hourly_usd_per_replica"}, map[string]any{
		"label": text, "model": text, "engine": text, "engine_version": text, "image_digest": text, "args": texts, "env": map[string]any{"type": "object", "additionalProperties": text},
		"accelerator": text, "accelerator_count": maybeInt, "provider": text, "region": text,
		"hourly_usd_per_replica": map[string]any{"type": []any{"number", "null"}, "description": "The hourly price of one replica recorded with the measurement: the customer's own compute price, never an InferCrane price. Null when none was recorded."},
		"price_source":           text,
	}), map[string]any{"type": "null"}}}
	number := object([]string{"key", "label", "unit", "higher_is_better", "before", "after", "change_percent", "noise_floor_percent", "decision", "basis"}, map[string]any{
		"key": text, "label": text, "unit": text, "higher_is_better": map[string]any{"type": "boolean"}, "before": maybe, "after": maybe, "change_percent": maybe, "noise_floor_percent": maybe,
		"decision": map[string]any{"type": "string", "enum": []string{"improved", "regressed", "no_measurable_difference", "unchanged", "unknown"}}, "basis": text, "reason": text})
	target := object([]string{"key", "label", "unit", "limit", "at_most", "before", "after", "before_met", "after_met"}, map[string]any{
		"key": map[string]any{"type": "string", "enum": []string{"ttft_p95_ms", "tpot_p95_ms", "error_rate", "output_tokens_per_second"}}, "label": text, "unit": text,
		"limit": map[string]any{"type": "number"}, "at_most": map[string]any{"type": "boolean"}, "before": maybe, "after": maybe, "before_met": maybeBool, "after_met": maybeBool, "explanation": text})
	quality := object([]string{"verdict", "basis", "prompts", "before_score", "after_score", "delta", "servers_each"}, map[string]any{
		"verdict": map[string]any{"type": "string", "enum": []string{"PASS", "WATCH", "BLOCK", "UNKNOWN"}}, "basis": text, "method": text, "eval": text, "prompts": maybeInt,
		"before_score": maybe, "after_score": maybe, "delta": maybe,
		"answer_rule":    map[string]any{"type": "string", "description": "How the task set was read: held_within_1_point for a setup that changes answers, within_margin otherwise."},
		"answer_verdict": text, "servers_each": maybeInt, "reason": text})
	claim := object([]string{"claim", "change_percent", "noise_floor_percent", "missing", "disclosures"}, map[string]any{
		"claim": map[string]any{"type": "string", "enum": []string{"shown", "not shown"}}, "technique": text, "metric": text, "change_percent": maybe, "noise_floor_percent": maybe,
		"missing":     map[string]any{"type": "array", "items": text, "description": "What a \"shown\" claim still needs, in words."},
		"disclosures": map[string]any{"type": "array", "items": text, "description": "The sentences a technique's claim carries, word for word from its measured result; empty for a setup no technique made. Show them wherever the claim is shown."}})
	saving := object([]string{"state", "reasons", "output_per_gpu_change_percent", "cost_per_million_change_percent", "gpu_hours_change_percent", "monthly_output_tokens", "monthly_gpu_hours_before", "monthly_gpu_hours_after", "monthly_cost_before_usd", "monthly_cost_after_usd", "monthly_saving_usd", "annual_saving_usd"}, map[string]any{
		"state":   map[string]any{"type": "string", "enum": []string{"verified", "not_shown", "none", "in_progress"}, "description": "verified only when the claim is shown, answers passed, no guard held the setup back, and output per GPU or cost per output token improved beyond the noise; the amounts are null otherwise."},
		"reasons": texts, "output_per_gpu_change_percent": maybe, "cost_per_million_change_percent": maybe, "gpu_hours_change_percent": maybe, "monthly_output_tokens": maybe,
		"monthly_gpu_hours_before": maybe, "monthly_gpu_hours_after": maybe, "monthly_cost_before_usd": maybe, "monthly_cost_after_usd": maybe, "monthly_saving_usd": maybe,
		"annual_saving_usd": map[string]any{"type": []any{"number", "null"}, "description": "Twelve months at the captured rate: a projection."},
		"assumption":        text, "monthly_unavailable_reason": text})
	record := object([]string{"kind", "digest"}, map[string]any{"kind": text, "id": text, "digest": text, "subject": text})
	run := object([]string{"campaign_id", "load", "finished", "outcome", "headline", "objective", "simulated_accelerator", "before", "after", "workload", "baseline", "numbers", "targets", "quality", "claim", "saving", "evidence", "rerun", "warnings"}, map[string]any{
		"campaign_id": text, "load": maybeInt, "finished": map[string]any{"type": "boolean"}, "outcome": map[string]any{"type": "string", "enum": []string{"winner_selected", "baseline_kept", "no_winner", "in_progress", "cancelled"}},
		"headline": text, "objective": text, "objective_metric": text, "simulated_accelerator": map[string]any{"type": "boolean"}, "before": setup, "after": setup,
		"workload": object([]string{"source", "requests_per_run"}, map[string]any{"source": text, "profile": text, "shape_digest": digest, "requests_per_run": maybeInt, "method": text}),
		"baseline": object([]string{"label", "basis", "control_runs", "candidate_runs", "noise_floor_percent", "fresh_servers"}, map[string]any{
			"label": text, "basis": text, "control_runs": map[string]any{"type": "integer"}, "candidate_runs": map[string]any{"type": "integer"}, "noise_floor_percent": maybe, "fresh_servers": map[string]any{"type": "boolean"}, "reason": text}),
		"numbers": map[string]any{"type": "array", "items": number}, "targets": map[string]any{"type": "array", "items": target}, "quality": quality, "claim": claim, "saving": saving,
		"evidence": object([]string{"report_digest", "report_path", "verify", "records"}, map[string]any{"report_digest": text, "report_path": text, "verify": text, "records": map[string]any{"type": "array", "items": record}}),
		"rerun":    map[string]any{"type": "array", "items": text, "description": "Commands that measure the run again from the same request; NEW_RUN is the ID the first prints."},
		"warnings": texts})
	loads := object([]string{"metric", "summary", "reason", "bands"}, map[string]any{
		"metric": text, "summary": map[string]any{"type": "string", "enum": []string{"one_setup", "setup_per_band", "incomplete"}}, "reason": text,
		"bands": map[string]any{"type": "array", "items": object([]string{"load", "decision", "reason"}, map[string]any{
			"load": map[string]any{"type": "integer"}, "decision": map[string]any{"type": "string", "enum": []string{"winner", "keep_current", "no_decision", "unknown"}}, "winner": text, "reason": text})}})
	workload := object([]string{"name", "model", "traffic", "verdict", "summary", "runs", "monthly_saving_usd", "output_per_gpu_change_percent", "cost_per_million_change_percent", "gpu_hours_change_percent"}, map[string]any{
		"name": text, "model": text, "traffic": text,
		"verdict": map[string]any{"type": "string", "enum": []string{"verified_at_every_load", "verified_at_some_loads", "not_shown", "no_saving", "in_progress"}},
		"summary": text, "runs": map[string]any{"type": "array", "items": run}, "loads": loads, "loads_reason": text,
		"monthly_saving_usd":            map[string]any{"type": []any{"number", "null"}, "description": "The smallest verified monthly saving across the workload's loads; null unless every load verified one."},
		"output_per_gpu_change_percent": maybe, "cost_per_million_change_percent": maybe, "gpu_hours_change_percent": maybe})
	report := object([]string{"schema_version", "algorithm_version", "title", "period", "finished", "summary", "claim_rule", "workloads", "limits", "unknown", "report_digest"}, map[string]any{
		"schema_version": map[string]any{"type": "string", "enum": []string{"infercrane.dev/proof-report/v1"}}, "algorithm_version": text, "title": text, "prepared_for": text,
		"period":   object([]string{"from", "to"}, map[string]any{"from": text, "to": text}),
		"finished": map[string]any{"type": "boolean"},
		"summary": object([]string{"headline", "workloads", "verified_workloads", "monthly_saving_usd", "annual_saving_usd", "output_per_gpu_change_percent"}, map[string]any{
			"headline": text, "workloads": map[string]any{"type": "integer"}, "verified_workloads": map[string]any{"type": "integer"},
			"monthly_saving_usd": map[string]any{"type": []any{"number", "null"}, "description": "The sum of every workload's verified monthly saving; saving_covers says how many workloads that is."},
			"annual_saving_usd":  maybe, "saving_covers": text, "saving_assumption": text, "output_per_gpu_change_percent": maybe}),
		"claim_rule": text, "workloads": map[string]any{"type": "array", "items": workload}, "limits": texts, "unknown": texts,
		"report_digest": map[string]any{"type": "string", "pattern": "^sha256:[0-9a-f]{64}$", "description": "SHA-256 of the compact report JSON with report_digest emptied; it is the last member."},
	})
	return map[string]any{
		"ProofReport": report,
		"ProofReportEnvelope": object([]string{"report", "signature"}, map[string]any{
			"report":          ref("ProofReport"),
			"signature":       map[string]any{"oneOf": []any{ref("OptimizationCampaignReportSignature"), map[string]any{"type": "null"}}, "description": "Ed25519 signature over report_digest by the control plane's passport signing key; null until every run has finished or without a key, never made up. Verify a saved response with `infercrane report proof verify FILE`."},
			"unsigned_reason": map[string]any{"type": "string", "enum": []string{"proof_not_finished", "signing_key_not_configured"}, "description": "Why signature is null; absent when the report is signed."},
		}),
	}
}

// proofReportQueryParameters are the Proof Report's query parameters.
func proofReportQueryParameters() []map[string]any {
	return []map[string]any{
		{"name": "campaigns", "in": "query", "required": true, "description": "1 to 16 finished campaign IDs, comma-separated: the runs of the engagement. Runs of one model on one traffic and GPU form one workload, one run per load.", "schema": map[string]any{"type": "string", "minLength": 1, "maxLength": 2048}},
		{"name": "title", "in": "query", "required": false, "description": "The report's title, one line of at most 120 characters. Defaults to Proof Report.", "schema": map[string]any{"type": "string", "maxLength": 120}},
		{"name": "prepared_for", "in": "query", "required": false, "description": "Who the report is prepared for, one line of at most 120 characters.", "schema": map[string]any{"type": "string", "maxLength": 120}},
		{"name": "format", "in": "query", "required": false, "description": "json (the default) returns the signed envelope; markdown and html return the same report rendered for print, as an attachment.", "schema": map[string]any{"type": "string", "enum": []string{"json", "markdown", "html"}}},
	}
}
