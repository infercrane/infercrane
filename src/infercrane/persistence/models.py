from __future__ import annotations

import uuid
from datetime import UTC, datetime

from sqlalchemy import DateTime, Float, ForeignKey, Integer, String, Text, UniqueConstraint
from sqlalchemy.orm import DeclarativeBase, Mapped, mapped_column, relationship


def utcnow() -> datetime:
    return datetime.now(UTC)


def new_id() -> str:
    return str(uuid.uuid4())


class Base(DeclarativeBase):
    pass


class DeploymentRow(Base):
    __tablename__ = "deployments"

    id: Mapped[str] = mapped_column(String(36), primary_key=True, default=new_id)
    name: Mapped[str] = mapped_column(String(128), unique=True, index=True)
    model: Mapped[str] = mapped_column(String(512))
    runtime: Mapped[str] = mapped_column(String(32))
    routing_strategy: Mapped[str] = mapped_column(String(64), default="round-robin")
    desired_state: Mapped[str] = mapped_column(String(32), default="running")
    observed_state: Mapped[str] = mapped_column(String(32), default="pending")
    created_at: Mapped[datetime] = mapped_column(DateTime(timezone=True), default=utcnow)
    updated_at: Mapped[datetime] = mapped_column(
        DateTime(timezone=True), default=utcnow, onupdate=utcnow
    )

    targets: Mapped[list[DeploymentTargetRow]] = relationship(
        back_populates="deployment", cascade="all, delete-orphan"
    )


class TargetRow(Base):
    __tablename__ = "targets"

    id: Mapped[str] = mapped_column(String(36), primary_key=True, default=new_id)
    name: Mapped[str] = mapped_column(String(128), unique=True, index=True)
    url: Mapped[str] = mapped_column(String(2048), unique=True)
    provider: Mapped[str] = mapped_column(String(64), default="existing")
    runtime: Mapped[str] = mapped_column(String(32))
    upstream_model_name: Mapped[str | None] = mapped_column(String(512), nullable=True)
    health: Mapped[str] = mapped_column(String(32), default="starting")
    provider_resource_id: Mapped[str | None] = mapped_column(String(512), nullable=True)
    provider_details_json: Mapped[str | None] = mapped_column(Text, nullable=True)
    created_at: Mapped[datetime] = mapped_column(DateTime(timezone=True), default=utcnow)
    updated_at: Mapped[datetime] = mapped_column(
        DateTime(timezone=True), default=utcnow, onupdate=utcnow
    )


class DeploymentTargetRow(Base):
    __tablename__ = "deployment_targets"
    __table_args__ = (UniqueConstraint("deployment_id", "target_id"),)

    deployment_id: Mapped[str] = mapped_column(
        ForeignKey("deployments.id", ondelete="CASCADE"), primary_key=True
    )
    target_id: Mapped[str] = mapped_column(
        ForeignKey("targets.id", ondelete="CASCADE"), primary_key=True
    )
    deployment: Mapped[DeploymentRow] = relationship(back_populates="targets")
    target: Mapped[TargetRow] = relationship()


class RequestRecordRow(Base):
    __tablename__ = "request_records"

    request_id: Mapped[str] = mapped_column(String(64), primary_key=True)
    deployment_id: Mapped[str] = mapped_column(ForeignKey("deployments.id"), index=True)
    target_id: Mapped[str | None] = mapped_column(ForeignKey("targets.id"), nullable=True)
    started_at: Mapped[datetime] = mapped_column(DateTime(timezone=True))
    completed_at: Mapped[datetime | None] = mapped_column(DateTime(timezone=True), nullable=True)
    status_code: Mapped[int | None] = mapped_column(Integer, nullable=True)
    latency_ms: Mapped[float | None] = mapped_column(Float, nullable=True)
    input_tokens: Mapped[int | None] = mapped_column(Integer, nullable=True)
    output_tokens: Mapped[int | None] = mapped_column(Integer, nullable=True)
    error_type: Mapped[str | None] = mapped_column(String(128), nullable=True)


class DeploymentEventRow(Base):
    __tablename__ = "deployment_events"

    id: Mapped[str] = mapped_column(String(36), primary_key=True, default=new_id)
    deployment_id: Mapped[str | None] = mapped_column(
        ForeignKey("deployments.id"), nullable=True, index=True
    )
    target_id: Mapped[str | None] = mapped_column(ForeignKey("targets.id"), nullable=True)
    event_type: Mapped[str] = mapped_column(String(128), index=True)
    summary: Mapped[str] = mapped_column(Text)
    payload_json: Mapped[str] = mapped_column(Text, default="{}")
    created_at: Mapped[datetime] = mapped_column(DateTime(timezone=True), default=utcnow)


class RouterGenerationRow(Base):
    __tablename__ = "router_generations"
    __table_args__ = (UniqueConstraint("deployment_id", "generation"),)

    id: Mapped[str] = mapped_column(String(36), primary_key=True, default=new_id)
    deployment_id: Mapped[str] = mapped_column(ForeignKey("deployments.id"), index=True)
    generation: Mapped[int] = mapped_column(Integer)
    strategy: Mapped[str] = mapped_column(String(64))
    worker_set_hash: Mapped[str] = mapped_column(String(64))
    internal_endpoint: Mapped[str] = mapped_column(String(256))
    status: Mapped[str] = mapped_column(String(32))
    created_at: Mapped[datetime] = mapped_column(DateTime(timezone=True), default=utcnow)
