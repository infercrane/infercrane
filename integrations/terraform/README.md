# InferCrane Terraform provider

Manage InferCrane logical deployments and SLO policies without making Terraform the owner of GPU
replicas, rollout workers, or long-running cloud operations. InferCrane remains the single mutation
owner for serving lifecycle changes.

The provider is included in the source repository, qualified in CI, and distributed as
multi-platform ZIPs on the matching InferCrane GitHub release. Terraform Registry publication is
deferred until the provider has an independent repository and release path.

## Build from source

```bash
git clone --branch v1.0.0-rc.1 https://github.com/infercrane/infercrane.git
cd infercrane/integrations/terraform
mkdir -p "$HOME/.local/share/infercrane/providers"
go build -o "$HOME/.local/share/infercrane/providers/terraform-provider-infercrane"
```

Until Registry publication, add a development override to `~/.terraformrc` using the absolute path
to that directory:

```hcl
provider_installation {
  dev_overrides {
    "registry.terraform.io/infercrane/infercrane" = "/Users/you/.local/share/infercrane/providers"
  }
  direct {}
}
```

The override is a local installation mechanism, not Registry qualification. With the override in
place, run `terraform plan` or `terraform apply` directly; `terraform init` still attempts Registry
version discovery for the unpublished source address.

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
      version = "1.0.0-rc.1"
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

Import a deployment with a custom stable alias using both identities:

```bash
terraform import infercrane_deployment.support support-deployment/support-production
```

The import form is `deployment-name/endpoint-name`. A name-only import selects the default endpoint
whose name matches the deployment.

See the [Terraform integration guide](../../docs/integrations/terraform.mdx) for installation,
imports, state behavior, and release safety.

## License

The provider is part of the InferCrane open-source core and is available under the repository's
[Apache License 2.0](../../LICENSE).
