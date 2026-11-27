# InferCrane delivery action

Use the same immutable InferCrane deployment contract in pull requests and protected delivery
environments. The action is the regression gate for model and setting changes: it proves a pull
request's serving change against the setup that serves today and fails the check on a regression.
It can also render a non-mutating plan, explicitly apply a specification, or prove that an expected
revision is active with a verified Inference Passport.

## Gate pull requests on the proof

When a pull request changes a model, an engine setting, or a serving setup, `proof-gate` measures
the spec on the base commit against the pull request's spec on fresh, isolated servers of one kind,
checks the proposed setup's answers against the current one's, and posts one comment with the
verdict: `PASS`, `WATCH`, `BLOCK`, or `UNKNOWN`. `BLOCK` and `UNKNOWN` fail the check; `WATCH` is
neutral unless `on-watch: fail`. A proof creates billable GPU servers up to `max-cost-usd`, so run it
on `pull_request`, never on `pull_request_target`.

```yaml
permissions:
  contents: read
  pull-requests: write

steps:
  - uses: actions/checkout@v7
    with:
      fetch-depth: 0
  - uses: infercrane/infercrane/actions/infercrane@RELEASE_WITH_PROOF_GATE
    with:
      mode: proof-gate
      spec: deploy/infercrane.yaml
      max-cost-usd: "20"
    env:
      INFERCRANE_URL: ${{ secrets.INFERCRANE_URL }}
      INFERCRANE_API_KEY: ${{ secrets.INFERCRANE_API_KEY }}
```

Set `campaign: RUN_ID` instead of `spec` to gate on a run that already exists. The verdict is
decided from the run's signed report, saved at `report-path`, so anyone can recompute it with
`infercrane optimize gate --file REPORT`. Try every verdict without a GPU:
`examples/proof-gate/dry-run.sh`.

## Plan in pull requests

```yaml
permissions:
  contents: read

steps:
  - uses: actions/checkout@v7
  - uses: infercrane/infercrane/actions/infercrane@v2.1.0
    with:
      mode: plan
      spec: deploy/infercrane.yaml
    env:
      INFERCRANE_URL: ${{ secrets.INFERCRANE_URL }}
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
      - uses: infercrane/infercrane/actions/infercrane@v2.1.0
        with:
          mode: apply
          spec: deploy/infercrane.yaml
          confirm-apply: "true"
          wait-timeout: 30m
        env:
          INFERCRANE_URL: ${{ secrets.INFERCRANE_URL }}
          INFERCRANE_API_KEY: ${{ secrets.INFERCRANE_API_KEY }}
```

A local wait timeout does not cancel the durable operation. The action returns the operation ID so
operators can resume inspection with the CLI or control API. A successful apply also returns the
stable application-facing endpoint through the `endpoint` action output.

## Verify a release

```yaml
- uses: infercrane/infercrane/actions/infercrane@v2.1.0
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
