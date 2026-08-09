from pathlib import Path

import yaml
from pydantic import BaseModel, Field


class ModelSpec(BaseModel):
    id: str


class RuntimeSpec(BaseModel):
    engine: str = "vllm"
    args: list[str] = Field(default_factory=list)


class ResourceSpec(BaseModel):
    gpu: str


class ProviderSpec(BaseModel):
    cloud: str
    region: str | None = None


class ScalingSpec(BaseModel):
    min_replicas: int = Field(default=1, ge=1)
    max_replicas: int = Field(default=1, ge=1)


class RoutingSpec(BaseModel):
    strategy: str = "round-robin"


class DeploymentFile(BaseModel):
    name: str
    model: ModelSpec
    runtime: RuntimeSpec = Field(default_factory=RuntimeSpec)
    resources: ResourceSpec
    provider: ProviderSpec
    scaling: ScalingSpec = Field(default_factory=ScalingSpec)
    routing: RoutingSpec = Field(default_factory=RoutingSpec)

    @classmethod
    def load(cls, path: Path) -> "DeploymentFile":
        with path.open() as handle:
            data = yaml.safe_load(handle)
        return cls.model_validate(data)

    def model_post_init(self, _context) -> None:
        if self.scaling.max_replicas < self.scaling.min_replicas:
            raise ValueError("scaling.max_replicas must be >= scaling.min_replicas")
