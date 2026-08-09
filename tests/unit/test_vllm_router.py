from infercrane.domain.models import RoutingStrategy
from infercrane.routers.base import RouterSpec
from infercrane.routers.vllm_router import VLLMRouterBackend


def test_router_command_uses_upstream_policy_and_disables_retries(settings):
    spec = RouterSpec(
        deployment_id="dep-1",
        strategy=RoutingStrategy.CACHE_AWARE,
        workers=("http://gpu-a:8000", "http://gpu-b:8000"),
        host="127.0.0.1",
        port=19001,
    )

    command = VLLMRouterBackend(settings).command("vllm-router", spec)

    assert command[command.index("--policy") + 1] == "cache_aware"
    assert command[command.index("--retry-max-retries") + 1] == "1"
    worker_index = command.index("--worker-urls")
    assert command[worker_index + 1 : worker_index + 3] == list(spec.workers)
