from infercrane.domain.spec import DeploymentFile


def test_declarative_deployment_file(tmp_path):
    path = tmp_path / "infercrane.yaml"
    path.write_text(
        """
name: qwen-prod
model:
  id: Qwen/Qwen3-8B
runtime:
  engine: vllm
  args: [--enable-prefix-caching]
resources:
  gpu: A40
provider:
  cloud: runpod
scaling:
  min_replicas: 1
  max_replicas: 4
routing:
  strategy: cache-aware
"""
    )
    spec = DeploymentFile.load(path)
    assert spec.model.id == "Qwen/Qwen3-8B"
    assert spec.scaling.max_replicas == 4
    assert spec.routing.strategy == "cache-aware"
