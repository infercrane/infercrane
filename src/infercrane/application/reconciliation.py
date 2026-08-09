from __future__ import annotations

import asyncio
import hashlib
import json

import httpx
from sqlalchemy import select

from infercrane.domain.models import ObservedState, RoutingStrategy, TargetHealth
from infercrane.gateway.routes import RouteDirectory, RouteSnapshot
from infercrane.persistence import Database
from infercrane.persistence.models import (
    DeploymentEventRow,
    DeploymentRow,
    RouterGenerationRow,
    TargetRow,
)
from infercrane.routers import RouterBackend, RouterSpec, RouterUnavailable
from infercrane.runtimes import VLLMRuntime
from infercrane.settings import Settings

from .services import ControlPlane


class Reconciler:
    def __init__(
        self,
        settings: Settings,
        database: Database,
        routes: RouteDirectory,
        router: RouterBackend,
        client: httpx.AsyncClient | None = None,
    ):
        self.settings = settings
        self.database = database
        self.control = ControlPlane(database)
        self.routes = routes
        self.router = router
        self.runtime = VLLMRuntime()
        self.client = client

    async def run(self, stop: asyncio.Event) -> None:
        while not stop.is_set():
            await self.reconcile_once()
            try:
                await asyncio.wait_for(stop.wait(), timeout=self.settings.health_interval_seconds)
            except TimeoutError:
                pass

    async def reconcile_once(self) -> None:
        owned = self.client is None
        client = self.client or httpx.AsyncClient()
        try:
            for view in self.control.list_deployments():
                resolved = self.control.resolve_deployment(view.name)
                healthy: list[TargetRow] = []
                for target in resolved.targets:
                    ok, models = await self.runtime.inspect_health(client, target.url)
                    expected = target.upstream_model_name or resolved.deployment.model
                    new_health = (
                        TargetHealth.HEALTHY.value
                        if ok and expected in models
                        else TargetHealth.UNHEALTHY.value
                    )
                    self._set_target_health(target.id, new_health)
                    if new_health == TargetHealth.HEALTHY.value:
                        target.health = new_health
                        healthy.append(target)

                if not healthy:
                    self.routes.remove(view.name)
                    self._set_deployment_state(view.id, ObservedState.UNHEALTHY.value)
                    continue

                worker_hash = self._worker_hash(view.routing_strategy, healthy)
                generation = self._active_generation(view.id)
                needs_restart = (
                    generation is None
                    or generation.worker_set_hash != worker_hash
                    or not self.router.is_running(view.id)
                )
                if needs_restart:
                    next_generation = (generation.generation + 1) if generation else 1
                    port = self.settings.router_start_port + next_generation
                    try:
                        endpoint = await self.router.start(
                            RouterSpec(
                                deployment_id=view.id,
                                workers=tuple(sorted(target.url for target in healthy)),
                                strategy=RoutingStrategy(view.routing_strategy),
                                host="127.0.0.1",
                                port=port,
                            )
                        )
                    except RouterUnavailable as exc:
                        self.routes.remove(view.name)
                        self._event(view.id, "router_failed", str(exc))
                        self._set_deployment_state(view.id, ObservedState.DEGRADED.value)
                        continue
                    generation = self._record_generation(
                        view.id,
                        next_generation,
                        view.routing_strategy,
                        worker_hash,
                        endpoint,
                    )
                upstream_model = healthy[0].upstream_model_name or view.model
                self.routes.put(
                    RouteSnapshot(
                        deployment_id=view.id,
                        alias=view.name,
                        upstream_model=upstream_model,
                        router_url=generation.internal_endpoint,
                    )
                )
                state = (
                    ObservedState.HEALTHY.value
                    if len(healthy) == len(resolved.targets)
                    else ObservedState.DEGRADED.value
                )
                self._set_deployment_state(view.id, state)
        finally:
            if owned:
                await client.aclose()

    @staticmethod
    def _worker_hash(strategy: str, targets: list[TargetRow]) -> str:
        value = json.dumps([strategy, sorted(target.url for target in targets)])
        return hashlib.sha256(value.encode()).hexdigest()

    def _set_target_health(self, target_id: str, health: str) -> None:
        with self.database.session() as session:
            row = session.get(TargetRow, target_id)
            if row and row.health != health:
                row.health = health
                session.add(
                    DeploymentEventRow(
                        target_id=target_id,
                        event_type=f"replica_{health}",
                        summary=f"Target {row.name} became {health}",
                    )
                )

    def _set_deployment_state(self, deployment_id: str, state: str) -> None:
        with self.database.session() as session:
            row = session.get(DeploymentRow, deployment_id)
            if row:
                row.observed_state = state

    def _active_generation(self, deployment_id: str) -> RouterGenerationRow | None:
        with self.database.session() as session:
            return session.scalar(
                select(RouterGenerationRow)
                .where(
                    RouterGenerationRow.deployment_id == deployment_id,
                    RouterGenerationRow.status == "active",
                )
                .order_by(RouterGenerationRow.generation.desc())
            )

    def _record_generation(
        self,
        deployment_id: str,
        generation: int,
        strategy: str,
        worker_hash: str,
        endpoint: str,
    ) -> RouterGenerationRow:
        with self.database.session() as session:
            for old in session.scalars(
                select(RouterGenerationRow).where(
                    RouterGenerationRow.deployment_id == deployment_id,
                    RouterGenerationRow.status == "active",
                )
            ):
                old.status = "retired"
            row = RouterGenerationRow(
                deployment_id=deployment_id,
                generation=generation,
                strategy=strategy,
                worker_set_hash=worker_hash,
                internal_endpoint=endpoint,
                status="active",
            )
            session.add(row)
            session.flush()
            return row

    def _event(self, deployment_id: str, event_type: str, summary: str) -> None:
        with self.database.session() as session:
            session.add(
                DeploymentEventRow(
                    deployment_id=deployment_id,
                    event_type=event_type,
                    summary=summary,
                )
            )
