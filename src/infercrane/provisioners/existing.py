from __future__ import annotations

from infercrane.domain.models import ProvisionedTarget

from .base import DeploymentSpec, Provisioner, ProvisionerStatus


class ExistingProvisioner(Provisioner):
    async def deploy(self, spec: DeploymentSpec) -> ProvisionedTarget:
        raise ValueError("existing targets must be registered with 'infercrane target add'")

    async def status(self, provider_resource_id: str) -> ProvisionerStatus:
        return ProvisionerStatus(state="external", provider_resource_id=provider_resource_id)

    async def stop(self, provider_resource_id: str) -> None:
        raise ValueError("InferCrane does not own existing targets")

    async def destroy(self, provider_resource_id: str) -> None:
        raise ValueError("InferCrane does not own existing targets")
