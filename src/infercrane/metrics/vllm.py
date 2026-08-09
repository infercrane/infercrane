from __future__ import annotations

import math
import re

import httpx

from infercrane.domain.models import MetricSnapshot

from .base import MetricsCollector

_LINE = re.compile(
    r"^(?P<name>[a-zA-Z_:][a-zA-Z0-9_:]*)(?:\{[^}]*\})?\s+"
    r"(?P<value>[-+]?(?:\d+(?:\.\d*)?|\.\d+)(?:[eE][-+]?\d+)?)"
)

_NAMES = {
    "requests_running": ("vllm:num_requests_running",),
    "requests_waiting": ("vllm:num_requests_waiting",),
    "kv_cache_usage": ("vllm:kv_cache_usage_perc", "vllm:gpu_cache_usage_perc"),
    "prefix_cache_queries": (
        "vllm:prefix_cache_queries",
        "vllm:prefix_cache_queries_total",
        "vllm:cached_request_prefix_tokens",
    ),
    "prefix_cache_hits": (
        "vllm:prefix_cache_hits",
        "vllm:prefix_cache_hits_total",
        "vllm:prefix_cache_hit_tokens",
    ),
    "prompt_tokens_total": ("vllm:prompt_tokens_total",),
    "generation_tokens_total": ("vllm:generation_tokens_total",),
}


def parse_vllm_metrics(text: str) -> MetricSnapshot:
    values: dict[str, list[float]] = {}
    for raw_line in text.splitlines():
        line = raw_line.strip()
        if not line or line.startswith("#"):
            continue
        match = _LINE.match(line)
        if not match:
            continue
        value = float(match.group("value"))
        if math.isfinite(value):
            values.setdefault(match.group("name"), []).append(value)

    resolved: dict[str, float | None] = {}
    gauges = {"requests_running", "requests_waiting", "kv_cache_usage"}
    for field, candidates in _NAMES.items():
        chosen = next((values[name] for name in candidates if name in values), None)
        if chosen is None:
            resolved[field] = None
        elif field in gauges:
            resolved[field] = max(chosen)
        else:
            resolved[field] = sum(chosen)
    return MetricSnapshot(raw=text, **resolved)


class VLLMMetricsCollector(MetricsCollector):
    def __init__(
        self,
        client: httpx.AsyncClient | None = None,
        timeout: float = 5.0,
        api_key: str | None = None,
    ):
        self._client = client
        self.timeout = timeout
        self.headers = {"authorization": f"Bearer {api_key}"} if api_key else {}

    async def collect(self, base_url: str) -> MetricSnapshot:
        if self._client:
            response = await self._client.get(
                f"{base_url.rstrip('/')}/metrics", timeout=self.timeout, headers=self.headers
            )
        else:
            async with httpx.AsyncClient(timeout=self.timeout) as client:
                response = await client.get(f"{base_url.rstrip('/')}/metrics", headers=self.headers)
        response.raise_for_status()
        return parse_vllm_metrics(response.text)
