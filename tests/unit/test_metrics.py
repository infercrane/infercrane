from infercrane.metrics import parse_vllm_metrics


def test_metrics_are_normalized_and_labeled_counters_are_summed():
    snapshot = parse_vllm_metrics(
        """
        vllm:num_requests_running 4
        vllm:num_requests_waiting 2
        vllm:kv_cache_usage_perc 0.71
        vllm:prompt_tokens_total{model_name="a"} 10
        vllm:prompt_tokens_total{model_name="b"} 20
        vllm:generation_tokens_total 7
        """
    )
    assert snapshot.requests_running == 4
    assert snapshot.requests_waiting == 2
    assert snapshot.kv_cache_usage == 0.71
    assert snapshot.prompt_tokens_total == 30


def test_missing_metrics_remain_none():
    snapshot = parse_vllm_metrics("vllm:num_requests_running 1\n")
    assert snapshot.requests_running == 1
    assert snapshot.prefix_cache_hits is None
