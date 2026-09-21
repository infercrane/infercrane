# Scaleway Brezel real-infrastructure evidence

## Qualification boundary

This evidence records the bounded InferCrane private-computer production
workflow completed on 2026-09-21. It qualifies only the named Scaleway host,
public edge, dedicated Brezel project, immutable CPU environments, connector,
and InferCrane integration below. It is not a general Brezel, Scaleway,
multi-host, GPU, or hostile-multitenancy qualification.

| Boundary | Qualified identity |
|---|---|
| Host | `brezel-benchmark` at `51.159.203.254` |
| Public edge | `https://sandbox.infercrane.com` |
| Brezel project | `infercrane-private-computers` |
| Coding environment | `envr_964f832968870a80ca50570b` |
| Evaluation environment | `envr_82e4e61dd60afc96f4aff2a4` |
| Model connector | `connr_7cd6a14266f1d50ccc90c299` |
| Customer boundary | InferCrane-authenticated private computers |

The scoped service token is stored only on the host in a `0600` file. Its
value was not read, copied into the repository, sent to the browser, or
included in this evidence.

## Directly observed public boundary

On 2026-09-21, a fresh request to
`https://sandbox.infercrane.com/readyz` returned HTTP `200` with
`{"status":"ready"}` over HTTP/2 through Caddy. The response included HSTS,
`Cache-Control: no-store`, a deny-all content security policy,
`X-Content-Type-Options: nosniff`, and frame denial.

An unauthenticated request to `/v1/capabilities` returned HTTP `401`. This is
the expected production boundary: readiness is public, while capabilities
require the dedicated server-side service credential and are consumed through
InferCrane.

## Operator-qualified workflow

The production workflow was run through InferCrane rather than directly from a
customer browser to Brezel. It passed:

1. Create InferCrane metadata and a durable Brezel workspace.
2. Create a computer from an approved immutable environment.
3. Stream command output and receive the terminal execution event.
4. Write and read a file through the InferCrane proxy.
5. Create and use a re-brokered HTTP preview.
6. Read sanitized execution receipt evidence.
7. Pause and resume while retaining workspace data.
8. Reach the approved InferCrane model through the immutable connector.
9. Record content-free usage events.
10. Delete the computer and confirm cleanup.

The InferCrane control service remained healthy during the run. Browser-facing
responses did not expose the Brezel service token, project credential, engine
ID, node capability, internal address, provider sandbox ID, workspace ID, or
preview lease token.

## Claims intentionally not made

This evidence does not qualify complete-host-loss recovery, encrypted backup
and restore, replicated storage, GPU passthrough, interactive PTY or SSH,
arbitrary OCI builds, public shared multitenancy, controlled arbitrary egress,
hardware attestation, host restart recovery, capacity exhaustion behavior, or
adversarial guest-escape resistance. Those require separately recorded drills
or architecture changes.
