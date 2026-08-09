from __future__ import annotations

import asyncio
import importlib
import re
import shlex
from typing import Any

import httpx

from infercrane.domain.models import ProvisionedTarget

from .base import DeploymentSpec, Provisioner, ProvisionerStatus


class SkyPilotUnavailable(RuntimeError):
    pass


class SkyPilotProvisioner(Provisioner):
    """Supported SkyPilot SDK calls isolated behind InferCrane's provider model."""

    def __init__(
        self,
        sky_module: Any | None = None,
        health_timeout_seconds: float = 900,
        worker_api_key: str = "infercrane",
    ):
        self._sky = sky_module
        self.health_timeout_seconds = health_timeout_seconds
        self.worker_api_key = worker_api_key

    @property
    def sky(self):
        if self._sky is None:
            try:
                self._sky = importlib.import_module("sky")
            except ImportError as exc:
                raise SkyPilotUnavailable(
                    "SkyPilot is not installed; install InferCrane with the 'skypilot' extra"
                ) from exc
        return self._sky

    async def deploy(self, spec: DeploymentSpec) -> ProvisionedTarget:
        cluster = self._cluster_name(spec.name)
        try:
            task = self.sky.Task(
                setup=f"python -m pip install 'vllm=={spec.runtime_version}'",
                run=self._run_command(spec),
                secrets={"INFERCRANE_WORKER_API_KEY": self.worker_api_key},
            )
            infra = spec.cloud if spec.region is None else f"{spec.cloud}/{spec.region}"
            task.set_resources(
                self.sky.Resources(infra=infra, accelerators=spec.gpu, ports=[str(spec.port)])
            )
            request_id = self.sky.launch(
                task,
                cluster_name=cluster,
                retry_until_up=True,
                _need_confirmation=False,
            )
            await asyncio.to_thread(self.sky.stream_and_get, request_id)
            endpoint_request = self.sky.endpoints(cluster, port=spec.port)
            endpoints = await asyncio.to_thread(self.sky.get, endpoint_request)
            endpoint = endpoints.get(spec.port) or endpoints.get(str(spec.port))
            if not endpoint:
                raise SkyPilotUnavailable(
                    f"SkyPilot did not expose port {spec.port} for {cluster}"
                )
            await self._wait_healthy(endpoint, spec.model)
        except SkyPilotUnavailable:
            raise
        except Exception as exc:
            raise SkyPilotUnavailable(
                f"SkyPilot failed to provision {cluster}: {exc}"
            ) from exc
        return ProvisionedTarget(
            name=f"{spec.name}-0",
            url=endpoint.rstrip("/"),
            provider_resource_id=cluster,
            upstream_model_name=spec.model,
            details={
                "cloud": spec.cloud,
                "region": spec.region,
                "gpu": spec.gpu,
                "runtime": spec.runtime,
                "runtime_version": spec.runtime_version,
                "runtime_args": spec.runtime_args,
                "generated_task": {
                    "infra": infra,
                    "accelerators": spec.gpu,
                    "ports": [str(spec.port)],
                    "run": self._run_command(spec),
                },
            },
        )

    async def status(self, provider_resource_id: str) -> ProvisionerStatus:
        request_id = self.sky.status(
            cluster_names=[provider_resource_id], refresh=self.sky.StatusRefreshMode.FORCE
        )
        records = await asyncio.to_thread(self.sky.get, request_id)
        if not records:
            return ProvisionerStatus(state="not_found", provider_resource_id=provider_resource_id)
        record = records[0]
        state = getattr(record.get("status"), "value", str(record.get("status", "unknown"))).lower()
        return ProvisionerStatus(
            state=state,
            provider_resource_id=provider_resource_id,
            details={
                "resources": record.get("resources_str"),
                "metadata": record.get("metadata", {}),
            },
        )

    async def stop(self, provider_resource_id: str) -> None:
        request_id = self.sky.stop(provider_resource_id)
        await asyncio.to_thread(self.sky.get, request_id)

    async def destroy(self, provider_resource_id: str) -> None:
        request_id = self.sky.down(provider_resource_id)
        await asyncio.to_thread(self.sky.get, request_id)

    @staticmethod
    def _cluster_name(name: str) -> str:
        slug = re.sub(r"[^a-z0-9-]+", "-", name.lower()).strip("-")
        return f"infercrane-{slug}"[:63].rstrip("-")

    @staticmethod
    def _run_command(spec: DeploymentSpec) -> str:
        args = " ".join(shlex.quote(arg) for arg in spec.runtime_args)
        return (
            f"vllm serve {shlex.quote(spec.model)} --host 0.0.0.0 --port {spec.port} "
            f"--served-model-name {shlex.quote(spec.model)} "
            f'--api-key "$INFERCRANE_WORKER_API_KEY" {args}'
        ).strip()

    async def _wait_healthy(self, endpoint: str, model: str) -> None:
        deadline = asyncio.get_running_loop().time() + self.health_timeout_seconds
        async with httpx.AsyncClient(timeout=10) as client:
            while asyncio.get_running_loop().time() < deadline:
                try:
                    response = await client.get(
                        f"{endpoint.rstrip('/')}/v1/models",
                        headers={"authorization": f"Bearer {self.worker_api_key}"},
                    )
                    ids = {item.get("id") for item in response.json().get("data", [])}
                    if response.status_code == 200 and model in ids:
                        return
                except (httpx.HTTPError, ValueError):
                    pass
                await asyncio.sleep(5)
        raise SkyPilotUnavailable(f"vLLM did not become healthy at {endpoint}")
