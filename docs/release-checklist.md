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
- [ ] All four archives, SHA-256 checksums, archive SBOMs, and multi-architecture image are present.
- [ ] The uploaded `image-digest-TAG` artifact identifies the exact multi-architecture image; its
      registry manifest carries BuildKit SBOM and maximum-mode provenance attestations.
- [ ] Image and archive vulnerability scans are reviewed.
- [ ] Documentation links, security reporting, examples, issue templates, and <=60 second demo are checked.
- [ ] Release notes list measured evidence and known limitations without unsupported performance claims.
- [ ] Replace every `pending` qualification field in
      [`release-notes-v0.1.0-rc.1.md`](release-notes-v0.1.0-rc.1.md) from sanitized evidence; do not
      publish the template's pending status.
- [ ] Tag points at the audited final commit and the GitHub release remains a draft until sign-off.
