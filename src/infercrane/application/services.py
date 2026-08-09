from __future__ import annotations

import json
from dataclasses import dataclass
from datetime import UTC, datetime, timedelta

from sqlalchemy import select
from sqlalchemy.exc import IntegrityError
from sqlalchemy.orm import selectinload

from infercrane.domain.models import (
    DeploymentCreate,
    DeploymentView,
    DesiredState,
    ObservedState,
    ProvisionedTarget,
    RoutingStrategy,
    TargetCreate,
    TargetHealth,
    TargetView,
)
from infercrane.persistence.database import Database
from infercrane.persistence.models import (
    DeploymentEventRow,
    DeploymentRow,
    DeploymentTargetRow,
    RequestRecordRow,
    TargetRow,
)


class ConflictError(ValueError):
    pass


class NotFoundError(ValueError):
    pass


@dataclass(frozen=True)
class DeploymentResolved:
    deployment: DeploymentRow
    targets: tuple[TargetRow, ...]


@dataclass(frozen=True)
class RequestStats:
    requests_per_second: float
    error_rate: float
    p50_latency_ms: float | None
    p95_latency_ms: float | None


class ControlPlane:
    def __init__(self, database: Database):
        self.database = database

    def add_target(self, spec: TargetCreate) -> TargetView:
        with self.database.session() as session:
            existing = session.scalar(
                select(TargetRow).where(
                    (TargetRow.name == spec.name) | (TargetRow.url == str(spec.url).rstrip("/"))
                )
            )
            if existing:
                same = (
                    existing.name == spec.name
                    and existing.url == str(spec.url).rstrip("/")
                    and existing.runtime == spec.runtime.value
                )
                if same:
                    return TargetView.model_validate(existing)
                raise ConflictError("target name or URL is already registered")
            row = TargetRow(
                name=spec.name,
                url=str(spec.url).rstrip("/"),
                provider=spec.provider,
                runtime=spec.runtime.value,
                upstream_model_name=spec.upstream_model_name,
                health=TargetHealth.STARTING.value,
            )
            session.add(row)
            try:
                session.flush()
            except IntegrityError as exc:
                raise ConflictError("target name or URL is already registered") from exc
            return TargetView.model_validate(row)

    def list_targets(self) -> list[TargetView]:
        with self.database.session() as session:
            rows = session.scalars(select(TargetRow).order_by(TargetRow.name)).all()
            return [TargetView.model_validate(row) for row in rows]

    def add_provisioned_target(self, target: ProvisionedTarget, provider: str) -> TargetView:
        view = self.add_target(
            TargetCreate(
                name=target.name,
                url=target.url,
                provider=provider,
                runtime="vllm",
                upstream_model_name=target.upstream_model_name,
            )
        )
        with self.database.session() as session:
            row = session.get(TargetRow, view.id)
            assert row is not None
            row.provider_resource_id = target.provider_resource_id
            row.provider_details_json = json.dumps(target.details, sort_keys=True)
            session.flush()
            return TargetView.model_validate(row)

    def create_deployment(self, spec: DeploymentCreate) -> DeploymentView:
        if not spec.targets:
            raise ValueError("at least one target is required")
        with self.database.session() as session:
            existing = session.scalar(select(DeploymentRow).where(DeploymentRow.name == spec.name))
            if existing:
                attached = {
                    dt.target.name
                    for dt in session.scalars(
                        select(DeploymentTargetRow)
                        .options(selectinload(DeploymentTargetRow.target))
                        .where(DeploymentTargetRow.deployment_id == existing.id)
                    )
                }
                if existing.model == spec.model and attached == set(spec.targets):
                    return DeploymentView.model_validate(existing)
                raise ConflictError(
                    f"deployment {spec.name!r} already exists with different configuration"
                )

            targets = session.scalars(
                select(TargetRow).where(TargetRow.name.in_(spec.targets))
            ).all()
            found = {target.name for target in targets}
            missing = sorted(set(spec.targets) - found)
            if missing:
                raise NotFoundError(f"unknown target(s): {', '.join(missing)}")
            if any(target.runtime != spec.runtime.value for target in targets):
                raise ValueError("all targets must use the deployment runtime")

            upstream_names = {target.upstream_model_name or spec.model for target in targets}
            if len(upstream_names) != 1:
                raise ValueError(
                    "Stage 1 requires one common upstream model name across attached targets"
                )

            row = DeploymentRow(
                name=spec.name,
                model=spec.model,
                runtime=spec.runtime.value,
                routing_strategy=spec.routing_strategy.value,
                desired_state=DesiredState.RUNNING.value,
                observed_state=ObservedState.PENDING.value,
            )
            session.add(row)
            session.flush()
            session.add_all(
                [
                    DeploymentTargetRow(deployment_id=row.id, target_id=target.id)
                    for target in targets
                ]
            )
            session.add(
                DeploymentEventRow(
                    deployment_id=row.id,
                    event_type="deployment_created",
                    summary=f"Deployment {row.name} created",
                    payload_json=json.dumps({"targets": sorted(found)}),
                )
            )
            session.flush()
            return DeploymentView.model_validate(row)

    def list_deployments(self) -> list[DeploymentView]:
        with self.database.session() as session:
            rows = session.scalars(
                select(DeploymentRow)
                .where(DeploymentRow.desired_state != DesiredState.DELETED.value)
                .order_by(DeploymentRow.name)
            ).all()
            return [DeploymentView.model_validate(row) for row in rows]

    def resolve_deployment(self, name: str) -> DeploymentResolved:
        with self.database.session() as session:
            deployment = session.scalar(
                select(DeploymentRow).where(
                    DeploymentRow.name == name,
                    DeploymentRow.desired_state == DesiredState.RUNNING.value,
                )
            )
            if not deployment:
                raise NotFoundError(f"deployment {name!r} not found")
            rows = session.scalars(
                select(DeploymentTargetRow)
                .options(selectinload(DeploymentTargetRow.target))
                .where(DeploymentTargetRow.deployment_id == deployment.id)
            ).all()
            return DeploymentResolved(
                deployment=deployment, targets=tuple(row.target for row in rows)
            )

    def set_routing(self, name: str, strategy: RoutingStrategy) -> DeploymentView:
        with self.database.session() as session:
            row = session.scalar(select(DeploymentRow).where(DeploymentRow.name == name))
            if not row or row.desired_state == DesiredState.DELETED.value:
                raise NotFoundError(f"deployment {name!r} not found")
            if row.routing_strategy != strategy.value:
                row.routing_strategy = strategy.value
                row.updated_at = datetime.now(UTC)
                session.add(
                    DeploymentEventRow(
                        deployment_id=row.id,
                        event_type="routing_changed",
                        summary=f"Routing changed to {strategy.value}",
                    )
                )
            session.flush()
            return DeploymentView.model_validate(row)

    def delete_deployment(self, name: str) -> None:
        with self.database.session() as session:
            row = session.scalar(select(DeploymentRow).where(DeploymentRow.name == name))
            if not row or row.desired_state == DesiredState.DELETED.value:
                return
            row.desired_state = DesiredState.DELETED.value
            row.observed_state = ObservedState.DELETED.value
            session.add(
                DeploymentEventRow(
                    deployment_id=row.id,
                    event_type="deployment_deleted",
                    summary=f"Deployment {name} deleted",
                )
            )

    def request_stats(self, deployment_id: str, window_seconds: int = 300) -> RequestStats:
        cutoff = datetime.now(UTC) - timedelta(seconds=window_seconds)
        with self.database.session() as session:
            rows = session.scalars(
                select(RequestRecordRow).where(
                    RequestRecordRow.deployment_id == deployment_id,
                    RequestRecordRow.started_at >= cutoff,
                )
            ).all()
        latencies = sorted(row.latency_ms for row in rows if row.latency_ms is not None)
        errors = sum(bool(row.error_type) or (row.status_code or 500) >= 400 for row in rows)
        return RequestStats(
            requests_per_second=len(rows) / window_seconds,
            error_rate=errors / len(rows) if rows else 0.0,
            p50_latency_ms=_percentile(latencies, 0.50),
            p95_latency_ms=_percentile(latencies, 0.95),
        )


def _percentile(values: list[float], quantile: float) -> float | None:
    if not values:
        return None
    index = (len(values) - 1) * quantile
    lower = int(index)
    upper = min(lower + 1, len(values) - 1)
    fraction = index - lower
    return values[lower] * (1 - fraction) + values[upper] * fraction
