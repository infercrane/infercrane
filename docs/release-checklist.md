# v0.1 release checklist

Do not create the release tag until every evidence field is linked to a durable log or artifact.
Use the [release acceptance record](release-acceptance.md) for the requirement-by-requirement manual
run. It is intentionally left pending until real evidence is attached.

- [ ] `make verify`, `make deadcode`, `make audit`, and `make test-container` pass from a clean checkout.
- [ ] Gate 0 elastic RunPod lifecycle acceptance passes, including restart/disconnect/failure recovery.
- [ ] Serverless zero-to-worker, warm, scale-to-zero, second cold start, streaming, cancellation, and deletion pass.
- [ ] Provider inventory proves zero leaked billable resources after each demo.
- [ ] AIPerf benchmark result and exact reproduction configuration are persisted and attached.
- [ ] Cold-start explanation is backed by provider worker evidence; unavailable substages remain explicit.
- [ ] Release Guard rejects the bad candidate and persisted explanation reproduces the decision.
- [ ] Clean-machine `brew install infercrane` acceptance passes with the final formula checksums.
- [ ] All four archives, SHA-256 checksums, SBOMs, and multi-architecture image are present.
- [ ] Image and archive vulnerability scans are reviewed.
- [ ] Documentation links, security reporting, examples, issue templates, and <=60 second demo are checked.
- [ ] Release notes list measured evidence and known limitations without unsupported performance claims.
- [ ] Tag points at the audited final commit and the GitHub release remains a draft until sign-off.
