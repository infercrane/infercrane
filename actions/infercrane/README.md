# InferCrane delivery action

Use the same immutable InferCrane deployment contract in pull requests and protected delivery
environments. The action can render a non-mutating plan, explicitly apply a specification, or prove
that an expected revision is active with a verified Inference Passport.

## Plan in pull requests

```yaml
permissions:
  contents: read

steps:
  - uses: actions/checkout@v7
  - uses: infercrane/infercrane/actions/infercrane@v1.0.0
    with:
      mode: plan
      spec: deploy/infercrane.yaml
    env:
      INFERCRANE_CONTROL_URL: ${{ secrets.INFERCRANE_CONTROL_URL }}
      INFERCRANE_API_KEY: ${{ secrets.INFERCRANE_API_KEY }}
```

## Apply from a protected environment

`apply` is intentionally fail-closed. Set `confirm-apply: "true"` and protect the job with a GitHub
Environment that requires the approvals appropriate for your organization.

```yaml
jobs:
  deploy:
    environment: production
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v7
      - uses: infercrane/infercrane/actions/infercrane@v1.0.0
        with:
          mode: apply
          spec: deploy/infercrane.yaml
          confirm-apply: "true"
          wait-timeout: 30m
        env:
          INFERCRANE_CONTROL_URL: ${{ secrets.INFERCRANE_CONTROL_URL }}
          INFERCRANE_API_KEY: ${{ secrets.INFERCRANE_API_KEY }}
```

A local wait timeout does not cancel the durable operation. The action returns the operation ID so
operators can resume inspection with the CLI or control API. A successful apply also returns the
stable application-facing endpoint through the `endpoint` action output.

## Verify a release

```yaml
- uses: infercrane/infercrane/actions/infercrane@v1.0.0
  with:
    mode: release-check
    deployment: support-production
    revision: REVISION_ID
```

The result and machine-readable evidence are written to `output-path`. Credentials are read from the
environment and are never accepted as action inputs or written to the output document.

By default, the action downloads the exact `version` release and verifies its SHA-256 checksum. It
never substitutes a binary already present on the runner. Set `cli-path` only when deliberately
using a separately provisioned binary.

See the [GitHub Actions guide](../../docs/integrations/github-actions.mdx) for permissions, protected
environment guidance, output contracts, and failure behavior.

## License

This action is part of the InferCrane open-source core and is available under the repository's
[Apache License 2.0](../../LICENSE).
