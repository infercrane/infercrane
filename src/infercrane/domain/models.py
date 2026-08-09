from __future__ import annotations

from datetime import datetime
from enum import StrEnum
from typing import Any

from pydantic import BaseModel, ConfigDict, Field, HttpUrl


class RuntimeKind(StrEnum):
    VLLM = "vllm"


class TargetHealth(StrEnum):
    STARTING = "starting"
    HEALTHY = "healthy"
    UNHEALTHY = "unhealthy"
    DRAINING = "draining"
    STOPPED = "stopped"


class DesiredState(StrEnum):
    RUNNING = "running"
    DELETED = "deleted"


class ObservedState(StrEnum):
    PENDING = "pending"
    HEALTHY = "healthy"
    DEGRADED = "degraded"
    UNHEALTHY = "unhealthy"
    DELETED = "deleted"


class RoutingStrategy(StrEnum):
    ROUND_ROBIN = "round-robin"
    CONSISTENT_HASH = "consistent-hash"
    POWER_OF_TWO = "power-of-two"
    CACHE_AWARE = "cache-aware"

    @property
    def router_value(self) -> str:
        return self.value.replace("-", "_")


class TargetCreate(BaseModel):
    name: str = Field(pattern=r"^[a-zA-Z0-9][a-zA-Z0-9_-]*$")
    url: HttpUrl
    runtime: RuntimeKind = RuntimeKind.VLLM
    provider: str = "existing"
    upstream_model_name: str | None = None


class DeploymentCreate(BaseModel):
    name: str = Field(pattern=r"^[a-zA-Z0-9][a-zA-Z0-9_-]*$")
    model: str
    targets: list[str]
    runtime: RuntimeKind = RuntimeKind.VLLM
    routing_strategy: RoutingStrategy = RoutingStrategy.ROUND_ROBIN


class TargetView(BaseModel):
    model_config = ConfigDict(from_attributes=True)
    id: str
    name: str
    url: str
    provider: str
    runtime: str
    upstream_model_name: str | None
    health: str
    provider_resource_id: str | None = None
    created_at: datetime


class DeploymentView(BaseModel):
    model_config = ConfigDict(from_attributes=True)
    id: str
    name: str
    model: str
    runtime: str
    routing_strategy: str
    desired_state: str
    observed_state: str
    created_at: datetime
    updated_at: datetime


class MetricSnapshot(BaseModel):
    requests_running: float | None = None
    requests_waiting: float | None = None
    kv_cache_usage: float | None = None
    prefix_cache_queries: float | None = None
    prefix_cache_hits: float | None = None
    prompt_tokens_total: float | None = None
    generation_tokens_total: float | None = None
    raw: str = ""


class ProvisionedTarget(BaseModel):
    name: str
    url: str
    provider_resource_id: str
    upstream_model_name: str
    details: dict[str, Any] = Field(default_factory=dict)
