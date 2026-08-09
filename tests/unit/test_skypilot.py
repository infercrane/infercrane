from __future__ import annotations

from enum import StrEnum

import pytest

from infercrane.provisioners import DeploymentSpec, SkyPilotProvisioner


class StatusRefreshMode(StrEnum):
    FORCE = "force"


class FakeTask:
    def __init__(self, setup: str, run: str, secrets: dict[str, str]):
        self.setup = setup
        self.run = run
        self.secrets = secrets
        self.resources = None

    def set_resources(self, resources):
        self.resources = resources


class FakeResources:
    def __init__(self, **kwargs):
        self.kwargs = kwargs


class FakeSky:
    Task = FakeTask
    Resources = FakeResources
    StatusRefreshMode = StatusRefreshMode

    def __init__(self):
        self.task = None
        self.cluster = None
        self.destroyed = None

    def launch(self, task, cluster_name, **kwargs):
        self.task = task
        self.cluster = cluster_name
        return "launch-request"

    def stream_and_get(self, request_id):
        assert request_id == "launch-request"
        return 1, object()

    def endpoints(self, cluster, port):
        assert cluster == self.cluster
        return ("endpoints", port)

    def get(self, request):
        if isinstance(request, tuple) and request[0] == "endpoints":
            return {request[1]: "http://203.0.113.10:8000"}
        return None

    def status(self, cluster_names, refresh):
        assert refresh == StatusRefreshMode.FORCE
        return "status-request"

    def stop(self, cluster):
        return ("stop", cluster)

    def down(self, cluster):
        self.destroyed = cluster
        return ("down", cluster)


class StubHealthSkyPilotProvisioner(SkyPilotProvisioner):
    async def _wait_healthy(self, endpoint: str, model: str) -> None:
        assert endpoint == "http://203.0.113.10:8000"
        assert model == "Qwen/Qwen3-8B"


@pytest.mark.asyncio
async def test_skypilot_deploy_builds_supported_sdk_task_and_returns_target():
    sky = FakeSky()
    provisioner = StubHealthSkyPilotProvisioner(sky)
    target = await provisioner.deploy(
        DeploymentSpec(
            name="qwen-prod",
            model="Qwen/Qwen3-8B",
            cloud="runpod",
            gpu="A40",
            runtime_args=["--max-model-len", "32768"],
        )
    )
    assert sky.cluster == "infercrane-qwen-prod"
    assert sky.task.resources.kwargs == {
        "infra": "runpod",
        "accelerators": "A40",
        "ports": ["8000"],
    }
    assert "vllm==0.23.0" in sky.task.setup
    assert sky.task.secrets == {"INFERCRANE_WORKER_API_KEY": "infercrane"}
    assert "$INFERCRANE_WORKER_API_KEY" in sky.task.run
    assert "--max-model-len 32768" in sky.task.run
    assert target.url == "http://203.0.113.10:8000"
    assert target.provider_resource_id == "infercrane-qwen-prod"


@pytest.mark.asyncio
async def test_skypilot_destroy_waits_for_sdk_request():
    sky = FakeSky()
    provisioner = StubHealthSkyPilotProvisioner(sky)
    await provisioner.destroy("infercrane-qwen-prod")
    assert sky.destroyed == "infercrane-qwen-prod"
