from __future__ import annotations

import json

import httpx
import pytest

from infercrane.application.reconciliation import Reconciler
from infercrane.domain.models import DeploymentCreate, TargetCreate
from infercrane.gateway import RouteDirectory, RouteSnapshot
from infercrane.routers.base import RouterBackend, RouterSpec


class FakeRouter(RouterBackend):
    def __init__(self):
        self.specs: list[RouterSpec] = []
        self.running: set[str] = set()

    async def start(self, spec: RouterSpec) -> str:
        self.specs.append(spec)
        self.running.add(spec.deployment_id)
        return f"http://router:{spec.port}"

    async def stop(self, deployment_id: str) -> None:
        self.running.discard(deployment_id)

    def is_running(self, deployment_id: str) -> bool:
        return deployment_id in self.running


@pytest.mark.asyncio
async def test_unhealthy_workers_are_excluded_and_membership_updates(control, database, settings):
    for name, port in (("gpu-a", 8101), ("gpu-b", 8102)):
        control.add_target(TargetCreate(name=name, url=f"http://127.0.0.1:{port}", runtime="vllm"))
    control.create_deployment(
        DeploymentCreate(
            name="qwen-prod",
            model="Qwen/Qwen3-8B",
            targets=["gpu-a", "gpu-b"],
        )
    )
    healthy_ports = {8101}

    async def handler(request: httpx.Request):
        if request.url.port not in healthy_ports:
            return httpx.Response(503)
        if request.url.path == "/health":
            return httpx.Response(200)
        return httpx.Response(
            200,
            content=json.dumps(
                {"object": "list", "data": [{"id": "Qwen/Qwen3-8B", "object": "model"}]}
            ),
        )

    client = httpx.AsyncClient(transport=httpx.MockTransport(handler))
    router = FakeRouter()
    routes = RouteDirectory()
    reconciler = Reconciler(settings, database, routes, router, client)

    await reconciler.reconcile_once()
    assert router.specs[-1].workers == ("http://127.0.0.1:8101",)
    assert control.resolve_deployment("qwen-prod").deployment.observed_state == "degraded"
    assert routes.get("qwen-prod") is not None

    healthy_ports.add(8102)
    await reconciler.reconcile_once()
    assert router.specs[-1].workers == (
        "http://127.0.0.1:8101",
        "http://127.0.0.1:8102",
    )
    assert control.resolve_deployment("qwen-prod").deployment.observed_state == "healthy"
    await client.aclose()


@pytest.mark.asyncio
async def test_no_healthy_worker_removes_route(control, database, settings):
    control.add_target(TargetCreate(name="gpu-a", url="http://127.0.0.1:8101"))
    control.create_deployment(
        DeploymentCreate(name="qwen-prod", model="Qwen/Qwen3-8B", targets=["gpu-a"])
    )

    async def handler(_request: httpx.Request):
        return httpx.Response(503)

    client = httpx.AsyncClient(transport=httpx.MockTransport(handler))
    routes = RouteDirectory()
    routes.put(
        RouteSnapshot(
            deployment_id="stale",
            alias="qwen-prod",
            upstream_model="old",
            router_url="http://old",
        )
    )
    reconciler = Reconciler(settings, database, routes, FakeRouter(), client)
    await reconciler.reconcile_once()
    assert routes.get("qwen-prod") is None
    assert control.resolve_deployment("qwen-prod").deployment.observed_state == "unhealthy"
    await client.aclose()
