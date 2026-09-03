# Scheduled Model API evidence renewal

`model-api-renewer` is the smallest production renewal loop for InferCrane's
directly operated MVP catalog:

- `deepseek-v4-flash` through DeepSeek
- `glm-5.2`, `glm-5.3`, and `glm-5.3-flash` through Z.ai

It is a one-shot process, not a server. On every invocation it reads the
current catalog evidence. If more than six hours remain, it exits without any
supplier request. Inside the renewal window it processes profiles
sequentially, running three buffered and three streaming samples with a
512-token cap and the unchanged 24-hour evidence TTL. A failure leaves that
profile unpublished and the process exits non-zero after attempting the other
approved profiles.

Supplier and control-plane credentials are read only from the Machine
environment. They are never command arguments or evidence fields. Raw
evidence and release manifests are append-only `0600` files beneath
`INFERCRANE_MODEL_API_RENEWAL_STATE_DIR`; the database stores the immutable
evidence reference and digest. The Fly Machine must therefore use
`--rootfs-persist always`. This is acceptable for the MVP but is not a
replicated evidence archive; move the same append-only artifacts to an
operator-owned object store before production scale.

## Required Fly secrets

The scheduled Machine belongs to the same app as the control plane and
inherits its existing Fly-only secrets:

- `INFERCRANE_API_KEY`
- `DEEPSEEK_API_KEY`
- `ZAI_API_KEY`

Do not copy these secrets into GitHub Actions or pass them with `--env`.

## Create the scheduled Machine

After the reviewed image is deployed, resolve its immutable image reference
and the two existing database secret-reference IDs. Then run:

```sh
fly machine run IMAGE_REF \
  -a infercrane-control \
  -r fra \
  --name infercrane-model-api-renewer \
  --schedule hourly \
  --restart no \
  --rootfs-persist always \
  --autostart=false \
  --skip-dns-registration \
  --vm-size shared-cpu-1x \
  --vm-memory 512 \
  --env INFERCRANE_URL=https://api.infercrane.com \
  --env INFERCRANE_MODEL_API_OPERATOR_WORKSPACE=global \
  --env INFERCRANE_MODEL_API_SERVING_PLAN=SERVING_PLAN_ID \
  --env INFERCRANE_MODEL_API_CANARY_WORKSPACE=CANARY_WORKSPACE_ID \
  --env INFERCRANE_MODEL_API_DEEPSEEK_CREDENTIAL_REFERENCE=DEEPSEEK_SECRET_REFERENCE_ID \
  --env INFERCRANE_MODEL_API_ZAI_CREDENTIAL_REFERENCE=ZAI_SECRET_REFERENCE_ID \
  infercrane-model-api-renewer
```

The image keeps its normal `infercrane-entrypoint`; because the scheduled
command is `infercrane-model-api-renewer` rather than `infercrane serve`, the
entrypoint immediately `exec`s the one-shot operator. The command creates no
service and no minimum-running Machine. Inspect its first scheduled run and
persisted evidence before relying on it:

```sh
fly machine list -a infercrane-control
fly logs -a infercrane-control --machine MACHINE_ID
fly machine status MACHINE_ID -a infercrane-control
```

Fly does not allow a scheduled Machine to be started manually. For a
deliberate pre-demo renewal, create a separately reviewed one-shot Machine
from the same immutable image and environment with `--restart no`,
`--rootfs-persist always`, and command `infercrane-model-api-renewer --force`.
Keep that stopped Machine until its evidence expires, then delete it. Do not
extend the evidence TTL to avoid renewal.
