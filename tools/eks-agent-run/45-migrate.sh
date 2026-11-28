#!/usr/bin/env bash
# Phase 4b. Steps 4 to 6 of the journey, the way the product offers a 5%
# canary today (docs/features/migration.mdx): the customer's own vLLM server
# is the baseline, connected as an OpenAI-compatible endpoint whose traffic
# goes through the control plane's gateway; a migration campaign measures
# setups on the cluster, re-measures a gain on fresh servers, and signs a
# verdict per setup; the verdict admits a 5% canary; then a rollback.
#
#   1. infercrane connect … --manage-traffic, then traffic through the gateway
#   2. migrate capture, migrate price (a test value), the graded eval set
#   3. optimize propose --migrate-from; every setup gets the shared-GPU memory
#      limit (rules, deviation 2); the campaign is posted, approved, watched
#   4. the verdicts, and whether a fresh-server confirmation ran
#   5. migrate canary --percent 5, traffic, and where it went
#   6. migrate rollback, traffic, and where it went
#
# Each step records its result in the state file and is skipped when run
# again, so the phase resumes after a fix. It creates nothing in AWS.
. "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

RUN_DIR="$STATE_DIR/run"
SOURCE_ENDPOINT=support-bot
SOURCE_URL="http://qwen3.$NAMESPACE_MODELS.svc:8000/v1"
EVAL_SET="$TOOL_DIR/eval/graded-arithmetic.json"
SETUPS=${EKS_PROOF_SETUPS:-2}
# The rules' cap is $5 of the rate card at the L4's list price. The rate card
# is the GPU instance's list price (env.sh), so the cap scales with it: on the
# A10G ($1.006 an hour in us-east-1) $5 was below the planner's sourced worst
# case of $6.04 (27 Nov). Rate-card dollars are a test value, not money spent;
# the cluster's own time limit bounds the real cost.
CAMPAIGN_MAX_COST_USD=$(awk "BEGIN { printf \"%.2f\", 5 * $PRICE_GPU_INSTANCE / 0.8048 }")
CAMPAIGN_WATCH_MINUTES=120

# icli_stdin FILE ARGS… runs the CLI in the control plane's pod with FILE as
# its standard input.
icli_stdin() {
  local file=$1
  shift
  kc --namespace "$NAMESPACE_HOSTED" exec -i deployment/infercrane -- "$@" <"$file"
}

# gateway_burst N TAG sends N chat requests for the source endpoint through
# the gateway and prints how many answered 200. Answers are not kept.
gateway_burst() {
  local n=$1 tag=$2 i ok=0 status
  for i in $(seq 1 "$n"); do
    printf '{"model":"%s","stream":%s,"max_tokens":%d,"messages":[{"role":"user","content":"%s request %d: write one sentence about the planet %s."}]}' \
      "$SOURCE_ENDPOINT" "$([ $((i % 4)) = 0 ] && echo true || echo false)" "$((32 + (i % 5) * 32))" "$tag" "$i" \
      "$(sed -n "$((i % 8 + 1))p" <<<$'Mercury\nVenus\nEarth\nMars\nJupiter\nSaturn\nUranus\nNeptune')" >"$RUN_DIR/burst.json"
    status=$(curl -sS --max-time 120 --cacert "$STATE_DIR/tls.crt" --resolve "$CONTROL_HOST:$CONTROL_LOCAL_PORT:127.0.0.1" \
      -H "Content-Type: application/json" -H "@$STATE_DIR/app-auth-header" -o /dev/null -w '%{http_code}' \
      --data-binary "@$RUN_DIR/burst.json" "https://$CONTROL_HOST:$CONTROL_LOCAL_PORT/v1/chat/completions" || true)
    [ "$status" = 200 ] && ok=$((ok + 1))
  done
  printf '%s' "$ok"
}

# served_requests NAMESPACE SELECTOR prints the sum of vllm:request_success_total
# over the pods the selector names, read inside each pod.
served_requests() {
  local namespace=$1 selector=$2 pod total=0 count
  for pod in $(kc --namespace "$namespace" get pods -l "$selector" --field-selector=status.phase=Running -o jsonpath='{.items[*].metadata.name}'); do
    count=$(kc --namespace "$namespace" exec "$pod" -- python3 -c 'import re, urllib.request
text = urllib.request.urlopen("http://127.0.0.1:8000/metrics", timeout=10).read().decode()
print(int(sum(float(m) for m in re.findall(r"^vllm:request_success_total\{[^}]*\} ([0-9.e+]+)$", text, re.M))))' 2>/dev/null || echo 0)
    total=$((total + count))
  done
  printf '%s' "$total"
}

done_step() { [ -n "$(state_get "MIG_$1")" ]; }

migrate_phase() {
  need_tools aws kubectl curl python3
  need_cluster
  guard_phase migrate 30
  trap stop_port_forwards EXIT
  mkdir -p "$RUN_DIR"
  open_control
  [ -s "$STATE_DIR/app-auth-header" ] || die "no application key yet; 35-deploy.sh makes it"
  local product status
  product=$(state_get GPU_PRODUCT)
  [ -n "$product" ] || die "the L4 is not shared yet; run 40-run.sh first"

  step "1. infercrane connect $SOURCE_URL --as $SOURCE_ENDPOINT --manage-traffic"
  if ! done_step CONNECTED; then
    # The server's key goes to the CLI on its standard input, never as an
    # argument; the CLI sends it once and the control plane stores it encrypted.
    if icli_stdin "$STATE_DIR/server-token" sh -c 'IFS= read -r SOURCE_TOKEN || [ -n "$SOURCE_TOKEN" ]; export SOURCE_TOKEN
exec env INFERCRANE_URL="$0" SSL_CERT_FILE=/tls/tls.crt infercrane connect "$1" --as "$2" --model "$3" --manage-traffic --token-env SOURCE_TOKEN --output json' \
      "https://$CONTROL_HOST:8443" "$SOURCE_URL" "$SOURCE_ENDPOINT" "$MODEL" >"$RUN_DIR/connect.json" 2>"$RUN_DIR/connect.err"; then
      check source_connected pass "$SOURCE_ENDPOINT connected: $(head -c 300 "$RUN_DIR/connect.json" | tr '\n' ' ')"
      state_set MIG_CONNECTED "$(date +%s)"
    else
      check source_connected fail "$(tail -c 400 "$RUN_DIR/connect.err")"
      die "connect failed"
    fi
  fi
  local ok
  ok=$(gateway_burst 60 capture)
  [ "$ok" = 60 ] && check source_gateway_traffic pass "60 of 60 requests for $SOURCE_ENDPOINT answered 200 through the gateway" ||
    check source_gateway_traffic fail "$ok of 60 requests for $SOURCE_ENDPOINT answered 200 through the gateway"

  step "2. Capture, price, eval set"
  local digest eval_digest
  if ! done_step DIGEST; then
    icli migrate capture "$SOURCE_ENDPOINT" --binding primary --window 1h --max-requests 200 --output json >"$RUN_DIR/capture.json" 2>"$RUN_DIR/capture.err" ||
      die "migrate capture failed: $(tail -c 400 "$RUN_DIR/capture.err")"
    digest=$(python3 -c 'import json, re, sys
text = open(sys.argv[1]).read()
found = re.findall(r"sha256:[0-9a-f]{64}", text) or ["sha256:" + d for d in re.findall(r"\b[0-9a-f]{64}\b", text)]
print(found[0] if found else "")' "$RUN_DIR/capture.json")
    [ -n "$digest" ] || die "the capture named no digest: $(head -c 400 "$RUN_DIR/capture.json")"
    state_set MIG_DIGEST "$digest"
  fi
  digest=$(state_get MIG_DIGEST)
  check source_captured pass "workload fingerprint $digest"
  # A contract price for the source, so the migration can compare cost: a
  # mid-priced closed API's, so a setup on the L4 can come out cheaper and
  # be rolled out. A test value, written down as one; never a price claim.
  icli migrate price "$SOURCE_ENDPOINT" primary --authority contract --input-usd 1.00 --output-usd 8.00 \
    --valid-until 2026-11-29 --source "EKS qualification test value" >"$RUN_DIR/price.txt" 2>&1 ||
    die "migrate price failed: $(tail -c 300 "$RUN_DIR/price.txt")"
  if ! done_step EVAL; then
    kc --namespace "$NAMESPACE_HOSTED" exec -i deployment/infercrane -- sh -c 'cat >/tmp/eval-set.json' <"$EVAL_SET"
    icli evaluation set upload /tmp/eval-set.json >"$RUN_DIR/eval-upload.txt" 2>&1 || die "eval set upload failed: $(tail -c 300 "$RUN_DIR/eval-upload.txt")"
    eval_digest=$(grep -o 'sha256:[0-9a-f]\{64\}' "$RUN_DIR/eval-upload.txt" | head -1)
    [ -n "$eval_digest" ] || die "the upload named no digest: $(head -c 300 "$RUN_DIR/eval-upload.txt")"
    state_set MIG_EVAL "$eval_digest"
  fi
  eval_digest=$(state_get MIG_EVAL)
  check eval_set_uploaded pass "graded eval set $eval_digest ($(python3 -c 'import json, sys; d = json.load(open(sys.argv[1])); print("%d prompts, %s" % (len(d["prompts"]), d["grading"]))' "$EVAL_SET"))"

  step "3. The migration campaign"
  local campaign
  if ! done_step CAMPAIGN; then
    # --include-simulated: the catalog marks the Kubernetes adapter's vLLM
    # compatibility "simulated" (internal/integration/catalog.go), and the
    # planner proposes nothing on it without this flag ("missing:
    # qualified_provider_runtime_compatibility", 27 Nov). The laptop proof of
    # 20 Nov passed it too. A deviation, recorded in the rules.
    icli optimize propose "$MODEL" --model-revision "$MODEL_REVISION" --provider kubernetes --gpu "$product" \
      --include-simulated \
      --runtimes vllm --migrate-from "$SOURCE_ENDPOINT/primary" --dialect openai-compatible \
      --workload-fingerprint "$digest" --eval-set "$eval_digest" --max-candidates "$SETUPS" \
      --output json >"$RUN_DIR/proposal.json" 2>"$RUN_DIR/proposal.err" ||
      die "optimize propose failed: $(tail -c 500 "$RUN_DIR/proposal.err")"
    # Deviation 2 of the rules: every setup's server shares the L4, so each
    # gets the same memory limit and context length, replacing its own.
    python3 - "$RUN_DIR/proposal.json" "$GPU_MEMORY_FRACTION" "$MAX_MODEL_LEN" "$RUN_DIR/campaign-request.json" <<'PY'
import json, sys
proposal = json.load(open(sys.argv[1]))
fraction, length, out = sys.argv[2], sys.argv[3], sys.argv[4]
if not proposal.get("candidates"):
    sys.exit("the proposal has no setups; missing: %s" % ", ".join(proposal.get("missing") or ["nothing named"]))
# Only setups for the plain Kubernetes adapter: this cluster runs no NVIDIA
# Dynamo. Since fix/dynamo-only-when-installed the planner leaves kubernetes-dynamo
# setups out unless Dynamo is configured or named; this filter still guards a
# proposal from an older control plane, which offered them (27 Nov).
kept = [c for c in proposal["candidates"] if (c.get("deployment") or {}).get("provider", {}).get("adapter") == "kubernetes"]
dropped = len(proposal["candidates"]) - len(kept)
if not kept:
    sys.exit("the proposal has no setup for the kubernetes adapter")
for rank, candidate in enumerate(kept, start=1):
    candidate["rank"] = rank
proposal["candidates"] = kept
if isinstance(proposal.get("starting_point"), dict) and proposal["starting_point"].get("candidate_id") not in [c["id"] for c in kept]:
    proposal["starting_point"]["candidate_id"] = kept[0]["id"]
if dropped:
    print("dropped %d setup(s) for another adapter (kubernetes-dynamo)" % dropped)
def limit(args):
    args = list(args or [])
    for flag, value in (("--gpu-memory-utilization", fraction), ("--max-model-len", length)):
        cleaned, skip = [], False
        for arg in args:
            if skip:
                skip = False
                continue
            if arg == flag:
                skip = True
                continue
            if arg.startswith(flag + "="):
                continue
            cleaned.append(arg)
        args = cleaned + [flag, value]
    return args
for candidate in proposal.get("candidates", []):
    runtime = candidate.setdefault("deployment", {}).setdefault("runtime", {})
    runtime["args"] = limit(runtime.get("args"))
print("%d setups: %s" % (len(proposal.get("candidates", [])), "; ".join(
    "%s %s" % (c.get("configuration_profile") or c.get("id", "")[:12], " ".join(c["deployment"]["runtime"]["args"])) for c in proposal.get("candidates", []))))
json.dump({"proposal": proposal, "intent": "new_endpoint"}, open(out, "w"))
PY
    status=$(curl -sS --max-time 60 --cacert "$STATE_DIR/tls.crt" --resolve "$CONTROL_HOST:$CONTROL_LOCAL_PORT:127.0.0.1" \
      -X POST -H "Content-Type: application/json" -H "@$STATE_DIR/auth-header" -H "Idempotency-Key: eks-qual-migration-$(state_get CREATED_AT)-$(printf '%s' "$eval_digest" | cut -c8-19)" \
      -o "$RUN_DIR/campaign-created.json" -w '%{http_code}' --data-binary "@$RUN_DIR/campaign-request.json" \
      "https://$CONTROL_HOST:$CONTROL_LOCAL_PORT/api/v1/optimization/campaigns")
    case "$status" in 200 | 201) ;; *) die "posting the campaign answered HTTP $status: $(head -c 500 "$RUN_DIR/campaign-created.json")" ;; esac
    campaign=$(python3 -c 'import json, sys; d = json.load(open(sys.argv[1])); print((d.get("campaign") or d).get("id", ""))' "$RUN_DIR/campaign-created.json")
    [ -n "$campaign" ] || die "the campaign has no id: $(head -c 300 "$RUN_DIR/campaign-created.json")"
    icli optimize approve "$campaign" --max-cost-usd "$CAMPAIGN_MAX_COST_USD" --expires-in 6h --yes >"$RUN_DIR/approve.txt" 2>&1 ||
      die "approve failed: $(tail -c 400 "$RUN_DIR/approve.txt")"
    state_set MIG_CAMPAIGN "$campaign"
    state_set MIG_CAMPAIGN_STARTED_AT "$(date +%s)"
  fi
  campaign=$(state_get MIG_CAMPAIGN)
  check campaign_started pass "campaign $campaign approved"
  local until=$((SECONDS + CAMPAIGN_WATCH_MINUTES * 60)) state=""
  while [ "$SECONDS" -lt "$until" ]; do
    guard_phase "migrate (watching campaign $campaign)" 10 >/dev/null
    icli optimize inspect "$campaign" --output json >"$RUN_DIR/campaign.json" 2>/dev/null || true
    state=$(python3 -c 'import json, sys
try:
    d = json.load(open(sys.argv[1]))
except ValueError:
    d = {}
d = d.get("campaign", d)
print(d.get("state", ""), d.get("phase") or d.get("step") or "")' "$RUN_DIR/campaign.json")
    say "  $(date +%H:%M:%S) $campaign: $state; servers: $(kc --namespace "$NAMESPACE_HOSTED" get pods -l app.kubernetes.io/managed-by=infercrane --no-headers 2>/dev/null | awk '{print $3}' | sort | uniq -c | tr '\n' ' ')"
    # The campaign's end states (internal/optimizationcampaign/state.go).
    case "${state%% *}" in qualified | guard_passed | rejected | inconclusive | promoted | observed | cancelled | failed | cleaned) break ;; esac
    sleep 60
  done
  icli optimize results "$campaign" --output json >"$RUN_DIR/campaign-results.json" 2>&1 || true
  state_set MIG_CAMPAIGN_ENDED_AT "$(date +%s)"
  case "${state%% *}" in
    failed | cancelled | cleaned | running | approved | ranked | "") check campaign_decided fail "campaign ended ${state:-without a readable state}" ;;
    *) check campaign_decided pass "campaign state $state after $((($(date +%s) - $(state_get MIG_CAMPAIGN_STARTED_AT)) / 60)) minutes" ;;
  esac

  step "4. Verdicts and the fresh-server confirmation"
  icli migrate verdicts "$campaign" --output json >"$RUN_DIR/verdicts.json" 2>&1 || true
  local verdict setup
  read -r verdict setup < <(python3 -c 'import json, sys
try:
    d = json.load(open(sys.argv[1]))
except ValueError:
    d = {}
items = d if isinstance(d, list) else d.get("verdicts", [])
for item in items:
    if str(item.get("verdict", "")).upper() == "PASS":
        print(item.get("id", ""), item.get("candidate_id") or item.get("candidate_run_id") or "")
        break' "$RUN_DIR/verdicts.json")
  [ -n "$verdict" ] && check migration_verdict pass "PASS verdict $verdict" ||
    check migration_verdict fail "no PASS verdict: $(head -c 400 "$RUN_DIR/verdicts.json")"
  if grep -q -E '"(retire_candidate|retire_control|confirmation)"' "$RUN_DIR/campaign.json" "$RUN_DIR/campaign-results.json" 2>/dev/null; then
    check fresh_server_confirmation pass "the campaign record holds a confirmation on fresh servers (campaign.json)"
  else
    check fresh_server_confirmation unknown "no confirmation in the record: no setup showed a gain, so none was re-measured (not exercised)"
  fi
  [ -n "$verdict" ] || die "no PASS verdict, so no canary; the rest is reported as not reached"

  step "5. migrate canary $SOURCE_ENDPOINT --percent 5"
  local before_source before_target after_source after_target
  # The migration docs' own step. "optimize activate" is the docs' other way
  # to take it, tried only when this one is refused; both are recorded.
  if icli migrate canary "$SOURCE_ENDPOINT" --verdict "$verdict" --percent 5 --output json >"$RUN_DIR/canary.json" 2>&1; then
    check canary_admitted pass "migrate canary admitted: $(head -c 300 "$RUN_DIR/canary.json" | tr '\n' ' ')"
  elif [ -n "$setup" ] && icli optimize activate "$campaign" --candidate "$setup" --yes --output json >"$RUN_DIR/activate.json" 2>&1; then
    check canary_admitted pass "migrate canary refused ($(tail -c 200 "$RUN_DIR/canary.json" | tr '\n' ' ')); optimize activate took the canary step"
  else
    check canary_admitted fail "migrate canary: $(tail -c 300 "$RUN_DIR/canary.json" | tr '\n' ' '); activate: $(tail -c 300 "$RUN_DIR/activate.json" 2>/dev/null | tr '\n' ' ')"
    die "the canary was refused"
  fi
  sleep 20
  before_source=$(served_requests "$NAMESPACE_MODELS" app=qwen3)
  before_target=$(served_requests "$NAMESPACE_HOSTED" app.kubernetes.io/managed-by=infercrane)
  ok=$(gateway_burst 200 canary)
  after_source=$(served_requests "$NAMESPACE_MODELS" app=qwen3)
  after_target=$(served_requests "$NAMESPACE_HOSTED" app.kubernetes.io/managed-by=infercrane)
  local to_source=$((after_source - before_source)) to_target=$((after_target - before_target))
  say "  200 requests, $ok answered 200: $to_source reached the customer's server, $to_target the InferCrane deployment"
  if [ "$to_target" -ge 4 ] && [ "$to_target" -le 20 ] && [ "$ok" = 200 ]; then
    check canary_share pass "$to_target of 200 (expected about 10 at 5%) reached the deployment; $to_source the customer's server"
  else
    check canary_share fail "$to_target of 200 reached the deployment, $to_source the customer's server, $ok answered 200"
  fi

  step "6. migrate rollback $SOURCE_ENDPOINT"
  icli migrate rollback "$SOURCE_ENDPOINT" --output json >"$RUN_DIR/rollback.json" 2>&1 ||
    check rollback_admitted fail "$(tail -c 400 "$RUN_DIR/rollback.json")"
  sleep 20
  before_source=$(served_requests "$NAMESPACE_MODELS" app=qwen3)
  before_target=$(served_requests "$NAMESPACE_HOSTED" app.kubernetes.io/managed-by=infercrane)
  ok=$(gateway_burst 60 rollback)
  after_source=$(served_requests "$NAMESPACE_MODELS" app=qwen3)
  after_target=$(served_requests "$NAMESPACE_HOSTED" app.kubernetes.io/managed-by=infercrane)
  to_source=$((after_source - before_source))
  to_target=$((after_target - before_target))
  if [ "$to_target" = 0 ] && [ "$ok" = 60 ]; then
    check rollback_all_to_source pass "after rollback 60 of 60 answered 200 and none reached the deployment ($to_source the customer's server)"
  else
    check rollback_all_to_source fail "after rollback $to_target of 60 reached the deployment, $ok answered 200"
  fi

  echo
  say "Next: tools/eks-agent-run/60-uninstall.sh ($(minutes_left) minutes left before the cluster must be gone)"
  stop_port_forwards
}

run_masked migrate_phase
