import unittest

from campaign_support import (
    WORKLOAD,
    WORKLOADS,
    apply_runtime_parity,
    build_evidence,
    canonical_digest,
    encoded_token_count,
    openrouter_snapshot,
    percentile,
    project_lane_economics,
    speculation_health,
    summarize_lane,
    synthetic_prompt_content,
    merge_candidate_runs,
    merge_prometheus_measurements,
    summarize_torch_trace,
    custom_kernel_gate,
    parse_sglang_startup,
)


class CampaignSupportTest(unittest.TestCase):
    def test_encoded_token_count_handles_multimodal_mapping(self):
        self.assertEqual(encoded_token_count({"input_ids": [[1, 2, 3]], "attention_mask": [[1, 1, 1]]}), 3)
        self.assertEqual(encoded_token_count([1, 2, 3, 4]), 4)

    def test_percentile_interpolates(self):
        self.assertEqual(percentile([1, 2, 3, 4], 0.5), 2.5)
        self.assertEqual(percentile([1, 2, 3, 4], 0.95), 3.8499999999999996)

    def test_synthetic_prompts_cache_only_explicit_session_prefix(self):
        first = synthetic_prompt_content(seed="body ", repeats=2, variant=1)
        second = synthetic_prompt_content(seed="body ", repeats=2, variant=2)
        self.assertNotEqual(first.split(". ", 1)[0], second.split(". ", 1)[0])
        shared = "stable session prefix"
        turn_one = synthetic_prompt_content(
            seed="body ", repeats=2, variant=1, shared_prefix=shared
        )
        turn_two = synthetic_prompt_content(
            seed="body ", repeats=2, variant=2, shared_prefix=shared
        )
        self.assertTrue(turn_one.startswith(shared))
        self.assertTrue(turn_two.startswith(shared))

    def test_openrouter_targets_ignore_unstable_endpoint(self):
        def endpoint(name, speed, latency, uptime, prompt, completion):
            return {
                "provider_name": name,
                "name": name,
                "pricing": {"prompt": prompt, "completion": completion},
                "throughput_last_30m": {"p50": speed},
                "latency_last_30m": {"p50": latency},
                "uptime_last_30m": uptime,
                "uptime_last_1d": uptime,
            }

        payload = {
            "data": {
                "id": "qwen/qwen3.8-27b",
                "endpoints": [
                    endpoint("cheap", 60, 500, 100, "0.0000001", "0.0000018"),
                    endpoint("fast", 90, 420, 100, "0.0000002", "0.000002"),
                    endpoint("unstable", 900, 20, 80, "0.00000001", "0.00000001"),
                ],
            }
        }
        snapshot = openrouter_snapshot(payload, captured_at="2026-09-23T00:00:00Z")
        self.assertEqual(snapshot["targets"]["price_floor"]["provider"], "cheap")
        self.assertEqual(snapshot["targets"]["fastest_p50"]["provider"], "fast")
        self.assertEqual(snapshot["targets"]["lowest_latency_p50"]["provider"], "fast")
        self.assertEqual(snapshot["targets"]["launch"]["minimum_p50_output_tokens_per_second"], 100)

    def test_lane_and_evidence_match_current_schema_shape(self):
        rows = [
            {
                "success": True,
                "concurrency": 1,
                "completion_tokens": 100,
                "prompt_tokens": 200,
                "prompt_token_match": True,
                "ttft_ms": 100 + index,
                "itl_ms": 8,
                "output_tokens_per_second": 125,
                "slo_pass": True,
            }
            for index in range(12)
        ]
        lane = summarize_lane(rows, 12, 3.6)
        candidate = {
            "candidate_id": "sglang-control",
            "runtime_id": "sglang-0.5.20",
            "recipe": {"args": []},
            "gpu_inventory": [
                {
                    "name": "NVIDIA H200",
                    "uuid": "GPU-test",
                    "compute_capability": "9.0",
                }
            ],
            "quality": [
                {"name": name, "passed": True}
                for name in (
                    "api_models",
                    "stream_usage",
                    "stream_done",
                    "stream_finish_reason",
                    "buffered_usage",
                    "reasoning_output",
                    "thinking_semantic",
                    "structured_output",
                    "tool_call",
                    "invalid_request_rejected",
                    "speculation_health",
                    "runtime_parity",
                    "gdn_long_state_semantics",
                )
            ],
            "lanes": [{**lane, "concurrency": concurrency} for concurrency in (1, 4, 8)],
            "error_rate": 0,
            "prompt_token_mismatch_rate": 0,
        }
        evidence = build_evidence([candidate])
        self.assertEqual(evidence["workload_digest"], canonical_digest(WORKLOAD))
        self.assertIn("NVIDIA H200", evidence["hardware_identity"])
        self.assertEqual(evidence["slo"]["max_itl_ms"], 12.0)
        self.assertNotIn("openrouter", evidence["selection_policy"]["id"])
        self.assertEqual(evidence["candidates"][0]["lanes"][0]["requests"], 12)
        self.assertTrue(evidence["candidates"][0]["quality_passed"])

    def test_economics_requires_real_utilization_for_target_margin(self):
        lane = {
            "input_tokens": 2000,
            "output_tokens": 1000,
            "measurement_seconds": 1.0,
            "aggregate_output_tokens_per_second": 1000.0,
        }
        result = project_lane_economics(
            lane,
            hourly_cost_usd=3.0,
            input_price_usd_per_million=0.11,
            output_price_usd_per_million=2.50,
            target_utilization=0.70,
            non_gpu_revenue_fraction=0.10,
            target_gross_margin=0.35,
        )
        self.assertGreater(result["projected_gross_margin"], 0.35)
        self.assertLess(result["target_margin_utilization"], 0.70)
        self.assertTrue(result["passes_target_margin_at_target_utilization"])

    def test_workload_suite_distinguishes_released_and_derived_inputs(self):
        self.assertEqual(WORKLOADS["public-long-prefill"]["input_tokens"], 24832)
        self.assertEqual(WORKLOADS["public-context-boundary-262k"]["input_tokens"], 258048)
        self.assertEqual(WORKLOADS["public-context-boundary-262k"]["concurrency_lanes"], [1])
        self.assertEqual(
            WORKLOADS["public-interactive"]["slo"]["source"],
            "pre_registered_internal_launch_objective",
        )
        agent = WORKLOADS["agent-prefix-reuse"]
        self.assertEqual(agent["source"]["release_state"], "aggregate_findings_published_trace_planned")
        self.assertIn("not a replay", agent["boundary"])

    def test_runtime_parity_is_fail_closed(self):
        rows = [
            {
                "candidate_id": "sglang-0520-control",
                "correctness_probes": [
                    {"id": "exact", "parity_mode": "exact", "output_sha256": "a"},
                    {"id": "thinking", "parity_mode": "semantic", "output_sha256": "x"},
                ],
                "quality": [],
            },
            {
                "candidate_id": "candidate-match",
                "correctness_probes": [
                    {"id": "exact", "parity_mode": "exact", "output_sha256": "a"},
                    {"id": "thinking", "parity_mode": "semantic", "output_sha256": "y"},
                ],
                "quality": [],
            },
            {
                "candidate_id": "candidate-drift",
                "correctness_probes": [
                    {"id": "exact", "parity_mode": "exact", "output_sha256": "b"}
                ],
                "quality": [],
            },
        ]
        apply_runtime_parity(rows)
        self.assertTrue(rows[0]["quality"][-1]["passed"])
        self.assertTrue(rows[1]["quality"][-1]["passed"])
        self.assertFalse(rows[2]["quality"][-1]["passed"])

    def test_independent_runs_merge_raw_samples_and_receipts(self):
        base = {
            "candidate_id": "candidate",
            "runtime_id": "runtime",
            "recipe": {"digest": "same"},
            "workload": {"concurrency_lanes": [1]},
            "quality": [{"name": "api_models", "passed": True}],
            "correctness_probes": [{"output_sha256": "a"}],
            "lanes": [{"concurrency": 1, "measurement_seconds": 10}],
            "request_samples": [
                {
                    "success": True,
                    "concurrency": 1,
                    "completion_tokens": 100,
                    "prompt_tokens": 200,
                    "ttft_ms": 100,
                    "itl_ms": 8,
                    "output_tokens_per_second": 125,
                    "slo_pass": True,
                    "prompt_token_match": True,
                }
            ],
        }
        merged = merge_candidate_runs([base, dict(base)], hourly_cost_usd=3.6)[0]
        self.assertEqual(merged["independent_runs"], 2)
        self.assertEqual(merged["lanes"][0]["requests"], 2)
        self.assertEqual(len(merged["run_receipts"]), 2)

    def test_prometheus_merge_sums_counters_and_replaces_gauges(self):
        first = {
            'sglang:spec_accept_length': 3.1,
            'sglang:prompt_tokens_total{model="qwen"}': 100,
            'sglang:ttft_seconds_bucket{le="1"}': 8,
        }
        second = {
            'sglang:spec_accept_length': 3.6,
            'sglang:prompt_tokens_total{model="qwen"}': 40,
            'sglang:ttft_seconds_bucket{le="1"}': 3,
        }
        merged = merge_prometheus_measurements(first, second)
        self.assertEqual(merged['sglang:spec_accept_length'], 3.6)
        self.assertEqual(merged['sglang:prompt_tokens_total{model="qwen"}'], 140)
        self.assertEqual(merged['sglang:ttft_seconds_bucket{le="1"}'], 11)

    def test_speculation_health_normalizes_sglang_and_vllm(self):
        sglang = speculation_health(
            "native_mtp",
            {"sglang:spec_accept_length": 3.1, "sglang:spec_accept_rate": 0.72},
        )
        self.assertTrue(sglang["passed"])
        self.assertEqual(sglang["accept_length"], 3.1)
        vllm = speculation_health(
            "native_mtp",
            {
                "vllm:spec_decode_num_accepted_tokens_total": 78_467,
                "vllm:spec_decode_num_draft_tokens_total": 111_678,
                "vllm:spec_decode_num_drafts_total": 37_226,
            },
        )
        self.assertTrue(vllm["passed"])
        self.assertAlmostEqual(vllm["accept_length"], 78_467 / 37_226)
        self.assertAlmostEqual(vllm["accept_rate"], 78_467 / 111_678)

    def test_profile_and_kernel_gate_use_measured_device_time(self):
        trace = {
            "traceEvents": [
                {
                    "ph": "X",
                    "cat": "kernel",
                    "name": "fused_sigmoid_gating_delta_rule_update_kernel",
                    "dur": 70,
                },
                {"ph": "X", "cat": "kernel", "name": "cutlass_gemm", "dur": 20},
                {"ph": "X", "cat": "kernel", "name": "memcpy", "dur": 10},
                {"ph": "X", "cat": "cuda_runtime", "name": "cudaLaunchKernel", "dur": 999},
            ]
        }
        profile = summarize_torch_trace(trace)
        self.assertEqual(profile["total_gpu_kernel_time_us"], 100)
        self.assertEqual(profile["families"][0]["name"], "gated_deltanet")
        gate = custom_kernel_gate(profile)
        self.assertTrue(gate["eligible"])
        self.assertEqual(gate["decision"], "build_bounded_custom_candidate")

        vendor_profile = summarize_torch_trace(
            {
                "traceEvents": [
                    {"ph": "X", "cat": "kernel", "name": "deep_gemm_sm90", "dur": 80},
                    {"ph": "X", "cat": "kernel", "name": "other", "dur": 20},
                ]
            }
        )
        vendor_gate = custom_kernel_gate(vendor_profile)
        self.assertEqual(
            vendor_gate["decision"], "benchmark_existing_implementations_first"
        )
        self.assertEqual(
            vendor_gate["custom_generation_state"],
            "deferred_until_existing_candidates_are_measured",
        )

    def test_startup_parser_preserves_phase_breakdown(self):
        log = (
            "Engine startup timings (s): load_weight=19.59, kv_cache_allocation=2.87, "
            "scheduler_e2e=203.38, cuda_graph={prefill=151.24, decode=0.00, "
            "target_verify=16.66, draft_prefill=1.52, draft_decode=2.26, "
            "draft_extend=0.00}, tokenizer_e2e=221.28"
        )
        parsed = parse_sglang_startup(log)
        self.assertTrue(parsed["available"])
        self.assertEqual(parsed["load_weight_seconds"], 19.59)
        self.assertEqual(parsed["cuda_graph_seconds"]["prefill"], 151.24)


if __name__ == "__main__":
    unittest.main()
