from __future__ import annotations

import httpx

from .base import RuntimeAdapter


class VLLMRuntime(RuntimeAdapter):
    def __init__(self, api_key: str | None = None):
        self.headers = {"authorization": f"Bearer {api_key}"} if api_key else {}

    async def inspect_health(self, client: httpx.AsyncClient, url: str) -> tuple[bool, set[str]]:
        try:
            health = await client.get(f"{url.rstrip('/')}/health", timeout=5, headers=self.headers)
            if health.status_code != 200:
                return False, set()
            models = await client.get(
                f"{url.rstrip('/')}/v1/models", timeout=5, headers=self.headers
            )
            if models.status_code != 200:
                return False, set()
            ids = {str(item["id"]) for item in models.json().get("data", []) if item.get("id")}
            return True, ids
        except (httpx.HTTPError, ValueError, TypeError):
            return False, set()
