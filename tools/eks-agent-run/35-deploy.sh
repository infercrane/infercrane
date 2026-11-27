#!/usr/bin/env bash
# Phase 3b. Steps 3 and 4 of the journey on the whole L4, with the commands
# docs/integrations/kubernetes.mdx gives a customer:
#
#   1. `infercrane deploy Qwen/Qwen3-8B --cloud kubernetes --gpu NVIDIA-L4`
#      through the control plane's Kubernetes provider (vLLM 0.34.0, the
#      product's pin), until the replica is ready;
#   2. the objects the provider made: Deployment, Service and the engine
#      NetworkPolicy, and that the policy is enforced (a pod without the
#      control plane's label cannot reach the engine; the control plane can);
#   3. chat requests through the control plane's gateway with an InferCrane
#      key, streamed and not; an unknown key is refused;
#   4. `infercrane delete`, and nothing of it is left.
#
# It creates nothing in AWS. Checks go to run/checks.tsv.
. "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

RUN_DIR="$STATE_DIR/run"
DEPLOYMENT=qwen3-8b
GATEWAY_REQUESTS=20
GATEWAY_STREAMED=5

# gateway METHOD PATH OUT BODY [HEADER_FILE] calls the control plane's gateway
# through the port-forward and prints the HTTP status.
gateway() {
  local header=${5:-$STATE_DIR/app-auth-header}
  curl -sS --max-time 180 --cacert "$STATE_DIR/tls.crt" --resolve "$CONTROL_HOST:$CONTROL_LOCAL_PORT:127.0.0.1" \
    -X "$1" -H "Content-Type: application/json" -H "@$header" -o "$3" -w '%{http_code}' --data-binary "@$4" \
    "https://$CONTROL_HOST:$CONTROL_LOCAL_PORT$2"
}

app_key() {
  [ -s "$STATE_DIR/app-auth-header" ] && return 0
  printf '{"name":"eks-qual-app","role":"viewer"}' >"$RUN_DIR/principal-request.json"
  local status
  status=$( (umask 077 && api POST /api/v1/principals "$RUN_DIR/principal.json" "$RUN_DIR/principal-request.json"))
  [ "$status" = 201 ] || die "creating the application key answered HTTP $status: $(head -c 300 "$RUN_DIR/principal.json")"
  (umask 077 && python3 -c 'import json, sys; sys.stdout.write("Authorization: Bearer " + json.load(open(sys.argv[1]))["credential"])' "$RUN_DIR/principal.json" >"$STATE_DIR/app-auth-header")
  rm -f "$RUN_DIR/principal.json"
}

# probe_engine LABEL URL runs a one-off pod with the label app=LABEL in the
# control plane's namespace and prints what it got from URL: an HTTP status,
# or "no-connection".
probe_engine() {
  local label=$1 url=$2 pod="probe-$1-$RANDOM"
  kc --namespace "$NAMESPACE_HOSTED" run "$pod" --image "$(image_ref)" --restart=Never --labels "app=$label" \
    --overrides='{"spec":{"nodeSelector":{"infercrane.dev/pool":"system"},"automountServiceAccountToken":false}}' \
    --command -- python3 -c 'import sys, urllib.request
try:
    print(urllib.request.urlopen(sys.argv[1], timeout=8).status)
except urllib.error.HTTPError as error:
    print(error.code)
except Exception:
    print("no-connection")' "$url" >/dev/null
  kc --namespace "$NAMESPACE_HOSTED" wait --for=jsonpath='{.status.phase}'=Succeeded "pod/$pod" --timeout=180s >/dev/null 2>&1 || true
  kc --namespace "$NAMESPACE_HOSTED" logs "pod/$pod" 2>/dev/null | tail -1
  kc --namespace "$NAMESPACE_HOSTED" delete pod "$pod" --wait=false >/dev/null 2>&1 || true
}

deploy_phase() {
  need_tools aws kubectl curl python3
  need_cluster
  guard_phase deploy 45
  trap stop_port_forwards EXIT
  mkdir -p "$RUN_DIR"
  open_control
  local product status
  product=$(state_get GPU_PRODUCT_WHOLE)
  [ -n "$product" ] || die "the install phase has not finished; run 30-install.sh"

  step "1. infercrane deploy $FULL_MODEL --cloud kubernetes --gpu $product (the docs' command)"
  state_set DEPLOY_STARTED_AT "$(date +%s)"
  if icli deploy "$FULL_MODEL" --name "$DEPLOYMENT" --cloud kubernetes --gpu "$product" \
    --idempotency-key "$DEPLOYMENT-initial" --output json >"$RUN_DIR/deploy.json" 2>"$RUN_DIR/deploy.err"; then
    check deploy_accepted pass "deploy accepted: $(python3 -c 'import json, sys; d = json.load(open(sys.argv[1])); print("endpoint %s, runtime %s, provider %s" % (d.get("endpoint"), d.get("runtime"), d.get("provider")))' "$RUN_DIR/deploy.json")"
  else
    check deploy_accepted fail "$(tail -c 400 "$RUN_DIR/deploy.err")"
    die "the control plane refused the deploy"
  fi
  local endpoint
  endpoint=$(json_field "$RUN_DIR/deploy.json" endpoint)
  [ -n "$endpoint" ] || endpoint=$DEPLOYMENT
  state_set FULL_ENDPOINT "$endpoint"

  step "2. Waiting for the replica (the first start pulls the vLLM image and $FULL_MODEL's 16.4 GB)"
  local until=$((SECONDS + 30 * 60)) state="" reason
  while [ "$SECONDS" -lt "$until" ]; do
    api GET "/api/v1/deployments/$DEPLOYMENT" "$RUN_DIR/deployment.json" >/dev/null || true
    # lifecycle_status.serving_state is "serving" once the gateway routes to
    # a ready replica (internal/controlapi/api.go, deploymentLifecycleStatus).
    state=$(python3 -c 'import json, sys
try:
    d = json.load(open(sys.argv[1]))
except ValueError:
    d = {}
status = d.get("lifecycle_status") or {}
operation = d.get("active_operation") or {}
state = status.get("serving_state") or ""
if str(operation.get("state", "")).lower() in ("failed", "error"):
    state = "failed"
print(state)' "$RUN_DIR/deployment.json")
    case "$state" in serving | failed) break ;; esac
    reason=$(kc --namespace "$NAMESPACE_HOSTED" get pods -l app.kubernetes.io/managed-by=infercrane \
      -o jsonpath='{range .items[*]}{.status.phase}/{.status.containerStatuses[0].state.waiting.reason}{" "}{end}' 2>/dev/null || true)
    say "  $(date +%H:%M:%S) deployment: ${state:-unknown}; pods: ${reason:-none}"
    sleep 30
  done
  state_set DEPLOY_READY_AT "$(date +%s)"
  kc --namespace "$NAMESPACE_HOSTED" get deployment,service,networkpolicy,pods -l app.kubernetes.io/managed-by=infercrane -o wide | tee "$RUN_DIR/deploy-objects.txt"
  case "$state" in
    serving) check deploy_ready pass "$DEPLOYMENT is $state after $((($(state_get DEPLOY_READY_AT) - $(state_get DEPLOY_STARTED_AT)) / 60)) minutes" ;;
    *)
      check deploy_ready fail "$DEPLOYMENT is ${state:-unknown} after 30 minutes"
      kc --namespace "$NAMESPACE_HOSTED" describe pods -l app.kubernetes.io/managed-by=infercrane | tail -40
      kc --namespace "$NAMESPACE_HOSTED" logs -l app.kubernetes.io/managed-by=infercrane --tail=80 || true
      die "the replica did not become ready; 90-delete.sh --yes, or fix and run this phase again"
      ;;
  esac
  local engine_pod
  engine_pod=$(kc --namespace "$NAMESPACE_HOSTED" get pods -l app.kubernetes.io/managed-by=infercrane -o jsonpath='{.items[0].metadata.name}')
  kc --namespace "$NAMESPACE_HOSTED" exec "$engine_pod" -- nvidia-smi \
    --query-gpu=name,driver_version,memory.total,memory.used --format=csv,noheader >"$RUN_DIR/deploy-gpu.txt" 2>&1 || true
  say "  GPU in the engine pod: $(cat "$RUN_DIR/deploy-gpu.txt")"
  kc --namespace "$NAMESPACE_HOSTED" get pod "$engine_pod" -o jsonpath='{.spec.containers[0].image}{"\n"}{.spec.containers[0].command}{" "}{.spec.containers[0].args}{"\n"}' >"$RUN_DIR/deploy-engine-command.txt"
  say "  engine: $(head -c 600 "$RUN_DIR/deploy-engine-command.txt")"

  step "3. The engine's NetworkPolicy, enforced"
  local service url probe
  service=$(kc --namespace "$NAMESPACE_HOSTED" get service -l app.kubernetes.io/managed-by=infercrane -o jsonpath='{.items[0].metadata.name}:{.items[0].spec.ports[0].port}')
  url="http://${service%%:*}.$NAMESPACE_HOSTED.svc:${service##*:}/health"
  if [ -n "$(kc --namespace "$NAMESPACE_HOSTED" get networkpolicy -l app.kubernetes.io/managed-by=infercrane -o name)" ]; then
    check engine_policy_present pass "NetworkPolicy beside the Deployment: $(kc --namespace "$NAMESPACE_HOSTED" get networkpolicy -l app.kubernetes.io/managed-by=infercrane -o name | tr '\n' ' ')"
  else
    check engine_policy_present fail "no NetworkPolicy beside the engine"
  fi
  probe=$(probe_engine infercrane "$url")
  [ "$probe" = 200 ] && check engine_reachable_from_control pass "a pod labelled app=infercrane gets HTTP 200 from $url" ||
    check engine_reachable_from_control fail "a pod labelled app=infercrane got \"$probe\" from $url"
  probe=$(probe_engine np-outsider "$url")
  [ "$probe" = no-connection ] && check engine_policy_enforced pass "a pod labelled app=np-outsider cannot connect to $url" ||
    check engine_policy_enforced fail "a pod labelled app=np-outsider got \"$probe\" from $url (the policy is not enforced)"

  step "4. $GATEWAY_REQUESTS chat requests through the gateway ($GATEWAY_STREAMED streamed)"
  app_key
  local i ok=0 stream body answer
  for i in $(seq 1 "$GATEWAY_REQUESTS"); do
    stream=false
    [ "$i" -le "$GATEWAY_STREAMED" ] && stream=true
    body="$RUN_DIR/gateway-request.json"
    answer="$RUN_DIR/gateway-answer-$i.out"
    printf '{"model":"%s","stream":%s,"max_tokens":64,"messages":[{"role":"user","content":"Request %s: name one planet of the solar system."}]}' "$endpoint" "$stream" "$i" >"$body"
    status=$(gateway POST /v1/chat/completions "$answer" "$body" || true)
    local complete=no
    if [ "$stream" = true ]; then
      grep -q 'data: \[DONE\]' "$answer" && complete=yes
    else
      grep -q '"choices"' "$answer" && complete=yes
    fi
    if [ "$status" = 200 ] && [ "$complete" = yes ]; then
      ok=$((ok + 1))
    else
      say "  request $i (stream $stream): HTTP $status $(head -c 200 "$answer")"
    fi
    # Only sizes are kept, never the answers' text.
    wc -c <"$answer" >"$answer.size" && rm -f "$answer"
  done
  [ "$ok" = "$GATEWAY_REQUESTS" ] && check gateway_serves pass "$ok of $GATEWAY_REQUESTS answered 200 with a complete answer ($GATEWAY_STREAMED streamed)" ||
    check gateway_serves fail "$ok of $GATEWAY_REQUESTS answered 200 with a complete answer"
  printf 'Authorization: Bearer ic_unknown_%s' "$(openssl rand -hex 16)" >"$RUN_DIR/unknown-key-header"
  status=$(gateway POST /v1/chat/completions "$RUN_DIR/unknown-key-answer.json" "$body" "$RUN_DIR/unknown-key-header" || true)
  [ "$status" = 401 ] && check gateway_refuses_unknown_key pass "an unknown key gets HTTP 401" ||
    check gateway_refuses_unknown_key fail "an unknown key got HTTP $status"
  api GET "/api/v1/endpoints/$endpoint" "$RUN_DIR/full-endpoint.json" >/dev/null || true

  step "5. infercrane delete $DEPLOYMENT"
  if icli delete "$DEPLOYMENT" --yes --wait >"$RUN_DIR/delete.txt" 2>&1; then
    check deploy_deleted pass "infercrane delete finished"
  else
    check deploy_deleted fail "$(tail -c 300 "$RUN_DIR/delete.txt")"
  fi
  local left="" until_gone=$((SECONDS + 300))
  while [ "$SECONDS" -lt "$until_gone" ]; do
    left=$(kc --namespace "$NAMESPACE_HOSTED" get deployment,service,networkpolicy,pods -l app.kubernetes.io/managed-by=infercrane -o name)
    [ -z "$left" ] && break
    sleep 10
  done
  [ -z "$left" ] && check deploy_objects_gone pass "no Deployment, Service, NetworkPolicy or pod of it is left" ||
    check deploy_objects_gone fail "left: $(printf '%s' "$left" | tr '\n' ' ')"

  echo
  say "Next: tools/eks-agent-run/40-run.sh ($(minutes_left) minutes left before the cluster must be gone)"
  stop_port_forwards
}

run_masked deploy_phase
