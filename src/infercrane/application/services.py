from __future__ import annotations

import json
from dataclasses import dataclass
from datetime import UTC, datetime

from sqlalchemy import select
from sqlalchemy.exc import IntegrityError
from sqlalchemy.orm import selectinload

from infercrane.domain.models import (
    DeploymentCreate,
    DeploymentView,
    DesiredState,
    ObservedState,
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
