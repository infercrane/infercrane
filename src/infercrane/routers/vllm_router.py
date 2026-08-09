from __future__ import annotations

import asyncio
import shutil
from asyncio.subprocess import Process

import httpx

from infercrane.settings import Settings

from .base import RouterBackend, RouterSpec


class RouterUnavailable(RuntimeError):
    pass


class VLLMRouterBackend(RouterBackend):
    """Supervise the upstream vLLM Router as an opaque external process."""

    def __init__(self, settings: Settings):
        self.settings = settings
        self._processes: dict[str, Process] = {}

    def command(self, binary: str, spec: RouterSpec) -> list[str]:
        """Build the pinned upstream CLI contract in one testable place."""
        return [
            binary,
            "--host",
            spec.host,
            "--port",
            str(spec.port),
            "--policy",
            spec.strategy.router_value,
            "--worker-urls",
            *spec.workers,
            "--api-key",
            self.settings.api_key,
            "--retry-max-retries",
            # Upstream defines 1 as a single attempt (zero retries).
            "1",
        ]

    async def start(self, spec: RouterSpec) -> str:
        await self.stop(spec.deployment_id)
        binary = shutil.which(self.settings.router_binary)
        if binary is None:
            raise RouterUnavailable(
                f"{self.settings.router_binary!r} is not installed; install the pinned vLLM Router binary"
            )
        args = self.command(binary, spec)
        process = await asyncio.create_subprocess_exec(
            *args,
            stdout=asyncio.subprocess.PIPE,
            stderr=asyncio.subprocess.PIPE,
        )
        self._processes[spec.deployment_id] = process
        endpoint = f"http://{spec.host}:{spec.port}"
        async with httpx.AsyncClient() as client:
            for _ in range(100):
                if process.returncode is not None:
                    stderr = await process.stderr.read() if process.stderr else b""
                    raise RouterUnavailable(
                        f"vLLM Router exited during startup: {stderr.decode(errors='replace')[-500:]}"
                    )
                try:
                    response = await client.get(f"{endpoint}/health", timeout=0.5)
                    if response.status_code < 500:
                        return endpoint
                except httpx.HTTPError:
                    pass
                await asyncio.sleep(0.1)
        await self.stop(spec.deployment_id)
        raise RouterUnavailable("vLLM Router did not become ready within 10 seconds")

    async def stop(self, deployment_id: str) -> None:
        process = self._processes.pop(deployment_id, None)
        if not process or process.returncode is not None:
            return
        process.terminate()
        try:
            await asyncio.wait_for(process.wait(), timeout=5)
        except TimeoutError:
            process.kill()
            await process.wait()

    def is_running(self, deployment_id: str) -> bool:
        process = self._processes.get(deployment_id)
        return process is not None and process.returncode is None
