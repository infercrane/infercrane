import pytest

from infercrane.application import ConflictError, ControlPlane, NotFoundError
from infercrane.domain.models import DeploymentCreate, RoutingStrategy, TargetCreate
from infercrane.persistence import Database


def target(name="gpu-a", url="http://127.0.0.1:8101", upstream=None):
    return TargetCreate(name=name, url=url, runtime="vllm", upstream_model_name=upstream)


def test_target_registration_is_idempotent(control):
    first = control.add_target(target())
    second = control.add_target(target())
    assert first.id == second.id
    assert len(control.list_targets()) == 1


def test_duplicate_target_conflict(control):
    control.add_target(target())
    with pytest.raises(ConflictError):
        control.add_target(target(url="http://127.0.0.1:8102"))


def test_deployment_creation_and_alias_resolution(control):
    control.add_target(target())
    created = control.create_deployment(
        DeploymentCreate(name="qwen-prod", model="Qwen/Qwen3-8B", targets=["gpu-a"])
    )
    resolved = control.resolve_deployment("qwen-prod")
    assert created.name == "qwen-prod"
    assert resolved.deployment.model == "Qwen/Qwen3-8B"
    assert [item.name for item in resolved.targets] == ["gpu-a"]


def test_unknown_target_rejected(control):
    with pytest.raises(NotFoundError):
        control.create_deployment(
            DeploymentCreate(name="qwen-prod", model="Qwen/Qwen3-8B", targets=["missing"])
        )


def test_deployment_persists_across_control_plane_restart(control, settings):
    control.add_target(target())
    control.create_deployment(
        DeploymentCreate(name="qwen-prod", model="Qwen/Qwen3-8B", targets=["gpu-a"])
    )
    restarted = ControlPlane(Database(settings))
    assert restarted.resolve_deployment("qwen-prod").deployment.model == "Qwen/Qwen3-8B"


def test_route_change_and_delete_are_idempotent(control):
    control.add_target(target())
    control.create_deployment(
        DeploymentCreate(name="qwen-prod", model="Qwen/Qwen3-8B", targets=["gpu-a"])
    )
    changed = control.set_routing("qwen-prod", RoutingStrategy.CACHE_AWARE)
    assert changed.routing_strategy == "cache-aware"
    control.delete_deployment("qwen-prod")
    control.delete_deployment("qwen-prod")
    with pytest.raises(NotFoundError):
        control.resolve_deployment("qwen-prod")
