from __future__ import annotations

from abc import ABC, abstractmethod

import httpx


class RuntimeAdapter(ABC):
    @abstractmethod
    async def inspect_health(
        self, client: httpx.AsyncClient, url: str
    ) -> tuple[bool, set[str]]: ...
