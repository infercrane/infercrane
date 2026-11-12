# Checkpoint catalog sync

`checkpoint-catalog-sync` refreshes the reviewed catalog of vendor-published
optimized checkpoints and speculators in
`internal/checkpointcatalog/catalog.json`. The optimizer and the control API
read only that committed file, which is compiled into the binary. This tool is
the only code that contacts Hugging Face, and only when you ask it to.

The flow has two steps:

1. `fetch --network` records a snapshot of public Hugging Face API responses.
2. `plan` turns a recorded snapshot into a proposed catalog and a reviewable
   diff. It never touches the network, and the same snapshot, policy, and
   committed catalog always produce the same output.

## Record a snapshot

```bash
go run ./tools/checkpoint-catalog-sync fetch --network --out .infercrane/checkpoint-catalog/snapshot
```

The policy's `selection` rule decides which builds the fetch observes. For each
publisher in `internal/checkpointcatalog/policy.json`, fetch lists the
organization twice: newest first back to the publisher's `created_after` date,
and most downloaded first down to `selection.min_downloads` downloads in the
last 30 days. It then reads every collection in `selection.collections` and
records the head of each member that no listing selected, when the member's
organization is a publisher in the policy. A publisher with `collection_only`
is never listed as an organization. A reviewed collection that returns an error
stops the fetch. Fetch then re-observes every repository pinned in the
committed catalog.

For each kept revision it records the revision record with file blobs, the
model card, `config.json`, `hf_quant_config.json`, and the license file. It
also records the head of every base model. The snapshot manifest records the
windows, threshold, and collections it was fetched with. The tool sends a
generic user agent and no credentials, honors the API rate-limit headers, and
refuses pagination links to any other host.

Add `--pin repository@revision` to observe an extra revision, for example to
keep a known-good revision while a newer head is reviewed.

## Plan and review

```bash
go run ./tools/checkpoint-catalog-sync plan --snapshot .infercrane/checkpoint-catalog/snapshot
```

Plan writes `catalog.json` and `catalog-diff.md` to `<snapshot>/plan`, which
the snapshot digest ignores, so planning a snapshot again never changes the
digest the catalog records. The
diff lists the entries that need review, the candidates, added builds with the
reason the selection rule picked each one, new revisions, changed and removed
entries, and the repositories that were skipped with the reason for each.

To approve a new publisher or format, resolve a license, or record the engine
release that contains a required pull request, edit the policy and plan the
same snapshot again. When the diff is right, install it:

```bash
go run ./tools/checkpoint-catalog-sync plan --snapshot .infercrane/checkpoint-catalog/snapshot --write --reviewed-at 2026-11-12
```

Commit the catalog together with any policy change.
`TestEmbeddedCatalogIsReviewedPinnedAndHardwareBound` fails when the committed
catalog was not regenerated after a policy edit.

## What the planner decides

- Format comes from `quantization_config` (ModelOpt, compressed-tensors, Quark,
  AutoRound) or `hf_quant_config.json`, and from card metadata for OpenVINO
  exports and speculators. Listing items need a quantization signal in the
  listing data to be fetched in detail. A collection member is fetched without
  one, and the planner then requires the signal in the files at the revision.
- Each build links to its declared base model. A card that states the exact
  source commit makes the lineage `exact`; otherwise the screening revision is
  the base head in the snapshot.
- Engine requirements come from the card. A link to an engine pull request
  blocks that engine until the policy records the release that contains it.
- Accuracy or speed tables in the card are copied verbatim as
  `publisher_priors` with `evidence_state: unmeasured`.
- A new revision of an approved publisher and format is re-reviewed
  automatically. It waits for a human only if its kind, base model, format,
  hardware vendor, or license changed. If the new revision is blocked, the
  previously pinned revision stays a candidate until the new one passes.
- Selection only decides what is observed. A build from a collection or above
  the download threshold passes the same review, license, engine, hardware,
  and lineage gates as any other.
- Only approved, licensed quantized checkpoints for NVIDIA architectures that
  InferCrane runs can become proposal candidates. AMD and Intel builds are
  recorded as `unsupported_hardware`.

The manifest digest hashes each file's path, size, and content identity (LFS
SHA-256 or Git blob ID) at the pinned revision.
