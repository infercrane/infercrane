# InferCrane Terraform provider

Manage InferCrane logical deployments and SLO policies without making Terraform the owner of GPU
replicas, rollout workers, or long-running cloud operations. InferCrane remains the single mutation
owner for serving lifecycle changes.

The provider is included in the source repository and is qualified in CI. Terraform Registry
publication is pending the first stable release. Until then, build it from the matching source tag or
use the InferCrane CLI and control API.

## Build from source

```bash
git clone --branch v1.0.0 https://github.com/infercrane/infercrane.git
cd infercrane/integrations/terraform
go build -o terraform-provider-infercrane
```

The provider requires:

- `INFERCRANE_CONTROL_URL`
- `INFERCRANE_API_KEY`

Credentials can also be configured in the provider block, but environment variables avoid storing a
secret in Terraform configuration.

## Example

```hcl
terraform {
  required_providers {
    infercrane = {
      source  = "infercrane/infercrane"
      version = "1.0.0"
    }
  }
}

provider "infercrane" {}

resource "infercrane_deployment" "support" {
  name         = "support-production"
  endpoint_name = "support-production"
  model        = "Qwen/Qwen3-8B"
  runtime      = "vllm"
  cloud        = "aws"
  compute_mode = "elastic"
  gpu          = "L40S"
  region       = "eu-central-1"
  min_replicas = 1
  max_replicas = 2
}

resource "infercrane_slo_policy" "support" {
  deployment       = infercrane_deployment.support.name
  max_ttft_p95_ms  = 500
  max_error_rate   = 0.01
}
```

Updates create and qualify an immutable candidate before promotion. If Release Guard requires
evidence that is not present, the update fails with an inspectable operation instead of silently
moving production traffic. A Terraform timeout does not cancel durable InferCrane work.

See the [Terraform integration guide](../../docs/integrations/terraform.mdx) for installation,
imports, state behavior, and release safety.

## License

The provider is part of the InferCrane open-source core and is available under the repository's
[MIT License](../../LICENSE).
