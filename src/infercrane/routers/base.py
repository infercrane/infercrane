from __future__ import annotations

from abc import ABC, abstractmethod
from dataclasses import dataclass

from infercrane.domain.models import RoutingStrategy


@dataclass(frozen=True)
class RouterSpec:
    deployment_id: str
    workers: tuple[str, ...]
    strategy: RoutingStrategy
    host: str
    port: int


class RouterBackend(ABC):
    @abstractmethod
    async def start(self, spec: RouterSpec) -> str: ...

    @abstractmethod
    async def stop(self, deployment_id: str) -> None: ...

    @abstractmethod
    def is_running(self, deployment_id: str) -> bool: ...
