from __future__ import annotations

from abc import ABC, abstractmethod

from infercrane.domain.models import MetricSnapshot


class MetricsCollector(ABC):
    @abstractmethod
    async def collect(self, base_url: str) -> MetricSnapshot: ...
