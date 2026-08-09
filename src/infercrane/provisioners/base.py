from __future__ import annotations

from abc import ABC, abstractmethod

from pydantic import BaseModel, Field

from infercrane.domain.models import ProvisionedTarget


class DeploymentSpec(BaseModel):
    name: str
    model: str
    runtime: str = "vllm"
    cloud: str
    gpu: str
    region: str | None = None
    runtime_version: str = "0.23.0"
    runtime_args: list[str] = Field(default_factory=list)
    port: int = 8000


class ProvisionerStatus(BaseModel):
    state: str
    provider_resource_id: str
    details: dict = Field(default_factory=dict)


class Provisioner(ABC):
    @abstractmethod
    async def deploy(self, spec: DeploymentSpec) -> ProvisionedTarget: ...

    @abstractmethod
    async def status(self, provider_resource_id: str) -> ProvisionerStatus: ...

    @abstractmethod
    async def stop(self, provider_resource_id: str) -> None: ...

    @abstractmethod
    async def destroy(self, provider_resource_id: str) -> None: ...
