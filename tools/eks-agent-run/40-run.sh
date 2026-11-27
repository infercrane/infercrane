#!/usr/bin/env bash
# Phase 4. The agent path, as a customer with a hosted control plane gets it,
# in the order of the agent's kind proof
# (internal/testtools/agentintegration/kind_test.go), on real hardware:
#
#   0. the operator shares the L4 four ways (time-slicing), the customer runs
#      their own model server, and installs the agent with its Helm chart and
#      the executor, as docs/architecture/agent.mdx "Kubernetes" says;
#   1. the agent found the customer's server and registered it;
#   2. the endpoint becomes agent-managed and the agent serves requests to it
#      inside the cluster, with an InferCrane API key;
#   3. one candidate run through the agent's command channel: start the
#      candidate in the customer's namespace, measure the current setup, the
#      candidate and the current setup again with AIPerf, run the answer check
#      on both, remove the candidate.
#
# It creates nothing in AWS. Every check is written to run/checks.tsv; the
# rules file says which of them the test needs to pass.
. "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

RUN_DIR="$STATE_DIR/run"

# data_plane METHOD PATH OUT BODY_FILE [HEADER_FILE] calls the agent's data
# plane through the port-forward and prints the HTTP status.
data_plane() {
  local header=${5:-$STATE_DIR/app-auth-header}
  curl -sS --max-time 120 -X "$1" -H "Content-Type: application/json" -H "@$header" \
    -o "$3" -w '%{http_code}' --data-binary "@$4" "http://127.0.0.1:$AGENT_LOCAL_PORT$2"
}

chat_body() {
  # chat_body STREAM writes a chat request for the endpoint.
  printf '{"model":"%s","stream":%s,"max_tokens":48,"messages":[{"role":"user","content":"Name one planet of the solar system."}]}' "$ENDPOINT_NAME" "$1"
}

# usage_of prints "prompt/completion tokens, N characters" for a chat answer.
# The answer's text is not kept.
usage_of() {
  python3 -c 'import json, sys
answer = json.load(open(sys.argv[1]))
usage = answer.get("usage") or {}
message = (answer.get("choices") or [{}])[0].get("message") or {}
text = (message.get("content") or "") + (message.get("reasoning_content") or "")
print("%s prompt and %s completion tokens, %d characters" % (usage.get("prompt_tokens"), usage.get("completion_tokens"), len(text)))' "$1"
}

gpu_snapshot() {
  # What the GPU itself reports, read inside the current server's container.
  kc --namespace "$NAMESPACE_MODELS" exec deployment/qwen3 -- nvidia-smi \
    --query-gpu=name,driver_version,memory.total,memory.used,utilization.gpu --format=csv,noheader 2>/dev/null || true
}

run_phase() {
  need_tools aws kubectl curl python3
  need_cluster
  guard_phase run 75
  # Only this run's check lines decide its exit code; earlier attempts stay in
  # the file (27 Nov: an earlier discovery failure made a passing rerun exit 6).
  local checks_start=0
  [ -f "$CHECKS" ] && checks_start=$(wc -l <"$CHECKS" | tr -d ' ')
  trap stop_port_forwards EXIT
  mkdir -p "$RUN_DIR"
  state_set RUN_STARTED_AT "$(date +%s)"
  open_control

  step "0a. The operator shares the L4: $GPU_SHARING_REPLICAS time-slices"
  local gpu_node product="" allocatable="" until_shared=$((SECONDS + 600))
  gpu_node=$(state_get GPU_NODE)
  [ -n "$gpu_node" ] || die "the install phase has not finished; run 30-install.sh"
  if [ -n "$(kc --namespace "$NAMESPACE_HOSTED" get deployments -l app.kubernetes.io/managed-by=infercrane -o name)" ]; then
    die "an InferCrane deployment still holds the whole GPU; 35-deploy.sh deletes it at its end"
  fi
  kc label node "$gpu_node" nvidia.com/device-plugin.config=shared --overwrite >/dev/null
  while [ "$SECONDS" -lt "$until_shared" ]; do
    product=$(kc get node "$gpu_node" -o jsonpath='{.metadata.labels.nvidia\.com/gpu\.product}')
    allocatable=$(kc get node "$gpu_node" -o jsonpath='{.status.allocatable.nvidia\.com/gpu}')
    [ "$allocatable" = "$GPU_SHARING_REPLICAS" ] && [ "$product" != "$(state_get GPU_PRODUCT_WHOLE)" ] && break
    sleep 10
  done
  if [ "$allocatable" = "$GPU_SHARING_REPLICAS" ]; then
    check gpu_shared pass "node $gpu_node: $allocatable nvidia.com/gpu, product label $product"
  else
    check gpu_shared fail "node $gpu_node shows $allocatable nvidia.com/gpu and product $product after 10 minutes"
    die "the L4 could not be shared"
  fi
  export GPU_PRODUCT="$product"
  state_set GPU_PRODUCT "$product"

  step "0b. The customer's own model server (namespace $NAMESPACE_MODELS): $MODEL on $product"
  # No trailing newline: the token goes into an HTTP header, where a newline
  # cannot travel, so the agent's probe of the server failed (27 Nov).
  secret_file "$STATE_DIR/server-token" sh -c 'openssl rand -hex 24 | tr -d "\n"'
  ensure_namespace "$NAMESPACE_MODELS"
  apply_secret "$NAMESPACE_MODELS" model-server-token "token=$STATE_DIR/server-token"
  render "$TOOL_DIR/manifests/models.yaml" | kc apply --dry-run=server -f - >/dev/null
  render "$TOOL_DIR/manifests/models.yaml" | kc apply -f -
  wait_model_server "$NAMESPACE_MODELS" qwen3 20

  step "0c. The agent key and the agent, with its Helm chart and the executor"
  local agent_id response="$STATE_DIR/agent-create.json"
  agent_id=$(state_get AGENT_ID)
  if [ -n "$agent_id" ] && kc --namespace "$NAMESPACE_MODELS" get secret infercrane-agent-key >/dev/null 2>&1 &&
    [ "$(api GET "/api/v1/agents/$agent_id" /dev/null)" = 200 ]; then
    say "  agent $(state_get AGENT_NAME) ($agent_id) and its key exist already"
  else
    local sequence name
    sequence=$(($(state_get AGENT_SEQUENCE || true) + 1))
    name="$AGENT_NAME-$sequence"
    printf '{"name":"%s"}' "$name" >"$STATE_DIR/agent-request.json"
    status=$( (umask 077 && api POST /api/v1/agents "$response" "$STATE_DIR/agent-request.json"))
    [ "$status" = 201 ] || [ "$status" = 200 ] || die "creating the agent key answered HTTP $status"
    # The key goes from the answer into the cluster's Secret and nowhere else.
    (umask 077 && python3 -c 'import json, sys; sys.stdout.write(json.load(open(sys.argv[1]))["key"])' "$response" >"$STATE_DIR/agent-key")
    agent_id=$(json_field "$response" agent.id)
    apply_secret "$NAMESPACE_MODELS" infercrane-agent-key "key=$STATE_DIR/agent-key"
    rm -f "$STATE_DIR/agent-key" "$response" "$STATE_DIR/agent-request.json"
    state_set AGENT_SEQUENCE "$sequence"
    state_set AGENT_NAME "$name"
    state_set AGENT_ID "$agent_id"
    say "  agent $name ($agent_id)"
  fi
  local image_args=() line
  while IFS= read -r line; do
    image_args+=("$line")
  done < <(helm_image_args)
  if helm upgrade --install "$HELM_RELEASE" "$REPO_DIR/deploy/helm/infercrane-agent" --namespace "$NAMESPACE_MODELS" \
    --values "$TOOL_DIR/values/agent.yaml" "${image_args[@]}" \
    --set controlPlane.url="https://$CONTROL_HOST:8443" --set-file controlPlane.caBundle="$STATE_DIR/tls.crt" \
    --set "agent.servers[0]=http://qwen3.$NAMESPACE_MODELS.svc:8000" \
    --set agent.endpointName="$ENDPOINT_NAME" --set agent.model="$MODEL" \
    --set "executor.allowedAccelerators[0]=$product" \
    --wait --timeout 5m; then
    check helm_install pass "release $HELM_RELEASE installed in $NAMESPACE_MODELS; agent ready"
  else
    kc --namespace "$NAMESPACE_MODELS" describe pods -l app.kubernetes.io/name=infercrane-agent | tail -25
    kc --namespace "$NAMESPACE_MODELS" logs -l app.kubernetes.io/name=infercrane-agent --tail=40 || true
    check helm_install fail "the agent's Helm release did not become ready"
    die "the agent's Helm release did not become ready"
  fi
  kc --namespace "$NAMESPACE_MODELS" get role,rolebinding,serviceaccount,secret,configmap,deployment,service \
    -l app.kubernetes.io/instance="$HELM_RELEASE" -o name >"$RUN_DIR/helm-objects.txt" || true
  say "  objects of the release: $(tr '\n' ' ' <"$RUN_DIR/helm-objects.txt")"

  step "1. Discovery: the agent registers the customer's server"
  local status until=$((SECONDS + 240))
  while :; do
    status=$(api GET "/api/v1/endpoints/$ENDPOINT_NAME" "$RUN_DIR/endpoint-observed.json")
    [ "$status" = 200 ] && break
    if [ "$SECONDS" -ge "$until" ]; then
      kc --namespace "$NAMESPACE_MODELS" logs -l app.kubernetes.io/name=infercrane-agent --tail=40 || true
      check discovery fail "endpoint $ENDPOINT_NAME was not registered within 4 minutes (HTTP $status)"
      die "the agent did not register the server"
    fi
    sleep 5
  done
  check discovery pass "endpoint $ENDPOINT_NAME registered by the agent"
  status=$(api GET "/api/v1/agents/$agent_id" "$RUN_DIR/agent.json")
  if [ "$status" = 200 ]; then
    python3 -c 'import json, sys
detail = json.load(open(sys.argv[1]))
agent = detail.get("agent", detail)
print("  agent: version %s, state %s" % (agent.get("version") or agent.get("agent_version"), agent.get("state")))
for server in detail.get("servers") or agent.get("servers") or []:
    print("  server: engine %s %s, model %s, health %s" % (server.get("engine"), server.get("engine_version"), server.get("upstream_model") or server.get("model"), server.get("health") or server.get("state")))
hardware = detail.get("hardware") or agent.get("hardware") or {}
print("  hardware seen by the agent pod: state %s, accelerators %s" % (hardware.get("state"), hardware.get("accelerators")))' "$RUN_DIR/agent.json" || true
    check agent_detail pass "agent detail read"
  else
    check agent_detail fail "GET /api/v1/agents/$agent_id answered HTTP $status"
  fi
  say "  GPU as the driver reports it: $(gpu_snapshot)"

  step "2. Data plane: the agent serves the endpoint inside the cluster"
  if [ ! -s "$STATE_DIR/app-auth-header" ]; then
    printf '{"name":"eks-proof-app","role":"viewer"}' >"$RUN_DIR/principal-request.json"
    status=$( (umask 077 && api POST /api/v1/principals "$RUN_DIR/principal.json" "$RUN_DIR/principal-request.json"))
    [ "$status" = 201 ] || die "creating the application key answered HTTP $status"
    (umask 077 && python3 -c 'import json, sys; sys.stdout.write("Authorization: Bearer " + json.load(open(sys.argv[1]))["credential"])' "$RUN_DIR/principal.json" >"$STATE_DIR/app-auth-header")
    rm -f "$RUN_DIR/principal.json"
  fi
  printf '{"ownership_mode":"agent-managed"}' >"$RUN_DIR/ownership-request.json"
  status=$(api PUT "/api/v1/adoptions/endpoints/$ENDPOINT_NAME/ownership" "$RUN_DIR/ownership.json" "$RUN_DIR/ownership-request.json")
  if [ "$status" = 200 ]; then
    check agent_managed pass "endpoint made agent-managed"
  else
    check agent_managed fail "PUT ownership answered HTTP $status: $(head -c 300 "$RUN_DIR/ownership.json")"
  fi
  api GET "/api/v1/endpoints/$ENDPOINT_NAME" "$RUN_DIR/endpoint-managed.json" >/dev/null
  if grep -q '"residency":"customer-cloud"' "$RUN_DIR/endpoint-managed.json"; then
    check residency pass "endpoint residency is customer-cloud"
  else
    check residency fail "endpoint residency is not customer-cloud"
  fi

  port_forward "$NAMESPACE_MODELS" "service/$HELM_RELEASE-infercrane-agent" "$AGENT_LOCAL_PORT" 8080 "http://127.0.0.1:$AGENT_LOCAL_PORT/livez"
  chat_body false >"$RUN_DIR/chat.json"
  chat_body true >"$RUN_DIR/chat-stream.json"
  until=$((SECONDS + 180))
  while :; do
    status=$(data_plane POST /v1/chat/completions "$RUN_DIR/answer.json" "$RUN_DIR/chat.json" || true)
    [ "$status" = 200 ] && break
    if [ "$SECONDS" -ge "$until" ]; then
      break
    fi
    sleep 5
  done
  if [ "$status" = 200 ]; then
    check data_plane_first pass "first request served by the agent: $(usage_of "$RUN_DIR/answer.json")"
  else
    check data_plane_first fail "the agent never served the endpoint (last HTTP $status: $(head -c 300 "$RUN_DIR/answer.json"))"
  fi
  local served=0 index body
  for index in 1 2 3 4 5 6; do
    body="$RUN_DIR/chat.json"
    [ $((index % 2)) -eq 0 ] && body="$RUN_DIR/chat-stream.json"
    status=$(data_plane POST /v1/chat/completions "$RUN_DIR/answer-$index.out" "$body" || true)
    if [ "$status" = 200 ] && { [ "$body" = "$RUN_DIR/chat.json" ] || grep -q '^data: \[DONE\]' "$RUN_DIR/answer-$index.out"; }; then
      served=$((served + 1))
    fi
  done
  rm -f "$RUN_DIR"/answer-*.out "$RUN_DIR/answer.json"
  if [ "$served" -eq 6 ]; then
    check data_plane_requests pass "6 of 6 requests served (3 streamed)"
  else
    check data_plane_requests fail "$served of 6 requests served"
  fi
  printf 'Authorization: Bearer ic_not_a_key' >"$RUN_DIR/wrong-header"
  status=$(data_plane POST /v1/chat/completions /dev/null "$RUN_DIR/chat.json" "$RUN_DIR/wrong-header" || true)
  rm -f "$RUN_DIR/wrong-header"
  if [ "$status" = 401 ]; then
    check data_plane_unknown_key pass "an unknown key gets HTTP 401"
  else
    check data_plane_unknown_key fail "an unknown key got HTTP $status"
  fi
  status=$(curl -sS --max-time 60 --cacert "$STATE_DIR/tls.crt" --resolve "$CONTROL_HOST:$CONTROL_LOCAL_PORT:127.0.0.1" \
    -X POST -H "Content-Type: application/json" -H "@$STATE_DIR/app-auth-header" -o "$RUN_DIR/hosted-refusal.json" -w '%{http_code}' \
    --data-binary "@$RUN_DIR/chat.json" "https://$CONTROL_HOST:$CONTROL_LOCAL_PORT/v1/chat/completions")
  if [ "$status" = 403 ] && grep -q residency_error "$RUN_DIR/hosted-refusal.json"; then
    check hosted_gateway_refuses pass "the control plane's own gateway refuses the endpoint (HTTP 403 residency_error)"
  else
    check hosted_gateway_refuses fail "the control plane's gateway answered HTTP $status"
  fi

  step "3. One candidate run through the agent"
  render "$TOOL_DIR/candidate-run.template.json" >"$RUN_DIR/candidate-spec.json"
  python3 -m json.tool "$RUN_DIR/candidate-spec.json" >/dev/null
  local run_id
  run_id=$(state_get CANDIDATE_RUN_ID)
  if [ -z "$run_id" ]; then
    status=$(api POST "/api/v1/agents/$agent_id/candidate-runs" "$RUN_DIR/candidate-queued.json" "$RUN_DIR/candidate-spec.json")
    if [ "$status" != 201 ]; then
      check candidate_queued fail "queueing answered HTTP $status: $(head -c 400 "$RUN_DIR/candidate-queued.json")"
      die "the candidate run was not queued"
    fi
    run_id=$(json_field "$RUN_DIR/candidate-queued.json" candidate_run.id)
    state_set CANDIDATE_RUN_ID "$run_id"
  fi
  check candidate_queued pass "candidate run $run_id queued ($LOAD_REQUESTS requests, $LOAD_CONCURRENCY at once, $LOAD_INPUT_TOKENS in, $LOAD_OUTPUT_TOKENS out)"

  local phase="" last_phase="" state="" started=$SECONDS latest
  latest=$(($(deadline) - DELETE_MINUTES * 60))
  until=$((SECONDS + CANDIDATE_TIMEOUT_SECONDS + 600))
  while :; do
    status=$(api GET "/api/v1/agents/$agent_id/candidate-runs/$run_id" "$RUN_DIR/candidate-run.json" || true)
    if [ "$status" = 200 ]; then
      state=$(json_field "$RUN_DIR/candidate-run.json" candidate_run.state)
      phase=$(json_field "$RUN_DIR/candidate-run.json" candidate_run.phase)
      if [ "$phase" != "$last_phase" ]; then
        say "  +$(((SECONDS - started) / 60))m$(((SECONDS - started) % 60))s  state $state, phase ${phase:-none}"
        last_phase=$phase
        case "$phase" in
          waiting_ready) kc --namespace "$NAMESPACE_MODELS" get pods -o wide | sed 's/^/      /' ;;
          measuring_candidate)
            say "      GPU while both servers are loaded: $(gpu_snapshot)"
            gpu_snapshot >"$RUN_DIR/gpu-both-servers.csv"
            kc --namespace "$NAMESPACE_MODELS" get pods -o wide >"$RUN_DIR/pods-both-servers.txt"
            ;;
        esac
      fi
      [ -n "$(json_field "$RUN_DIR/candidate-run.json" candidate_run.finished_at)" ] && break
    fi
    if [ "$SECONDS" -ge "$until" ]; then
      check candidate_finished fail "the candidate run did not finish within its time"
      die "the candidate run did not finish"
    fi
    if [ "$(date +%s)" -ge "$latest" ]; then
      check candidate_finished fail "stopped waiting: deleting must start now to stay inside the budget"
      say "REFUSED to wait longer. Run tools/eks-agent-run/90-delete.sh --yes now."
      exit 4
    fi
    sleep 15
  done
  state_set RUN_FINISHED_AT "$(date +%s)"

  if [ "$state" = succeeded ]; then
    check candidate_finished pass "candidate run succeeded in $(((SECONDS - started) / 60)) minutes"
  else
    check candidate_finished fail "candidate run ended $state: $(json_field "$RUN_DIR/candidate-run.json" candidate_run.failure_code) $(json_field "$RUN_DIR/candidate-run.json" candidate_run.failure_detail)"
    kc --namespace "$NAMESPACE_MODELS" get pods -o wide || true
    kc --namespace "$NAMESPACE_MODELS" get events --sort-by=.lastTimestamp | tail -15 || true
  fi
  python3 - "$RUN_DIR/candidate-run.json" "$CHECKS" <<'PY'
import json, sys
run = json.load(open(sys.argv[1]))["candidate_run"]
result = run.get("result") or {}
rows = []
def row(name, ok, detail):
    stamp = __import__("datetime").datetime.now(__import__("datetime").timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")
    rows.append("%s\t%s\t%s\t%s" % (stamp, name, "pass" if ok else "fail", detail))
    print("  %s  %s: %s" % ("PASS" if ok else "FAIL", name, detail))
for field in ("control", "candidate", "control_after"):
    m = result.get(field) or {}
    ok = bool(m) and m.get("failed") == 0 and (m.get("succeeded") or 0) > 0
    row("measured_" + field, ok, "%s of %s requests succeeded with %s %s" % (m.get("succeeded"), m.get("requests"), m.get("tool"), m.get("tool_version")) if m else "no measurement")
quality = result.get("quality") or {}
row("answer_check", bool(quality) and quality.get("verdict") in ("PASS", "WATCH", "BLOCK"), "verdict %s, score %s over %s prompts" % (quality.get("verdict"), quality.get("score"), quality.get("prompts")) if quality else "no answer check")
row("candidate_cleaned_up", result.get("cleaned_up") is True, "cleaned_up=%s, ready after %s s" % (result.get("cleaned_up"), result.get("ready_seconds")))
open(sys.argv[2], "a").write("\n".join(rows) + "\n")
PY
  if tail -n "+$((checks_start + 1))" "$CHECKS" | grep -q "	fail	"; then
    FAILED=1
  fi
  local remaining
  remaining=$(kc --namespace "$NAMESPACE_MODELS" get deployments,services -l app.kubernetes.io/managed-by=infercrane -o name)
  if [ -z "$remaining" ]; then
    check candidate_objects_gone pass "no candidate Deployment or Service is left in $NAMESPACE_MODELS"
  else
    check candidate_objects_gone fail "left behind: $remaining"
  fi

  step "Result"
  column -t -s "$(printf '\t')" "$CHECKS" | sed 's/^/  /'
  echo
  say "The numbers in $RUN_DIR/candidate-run.json describe two servers sharing one small GPU."
  say "They prove the path. They are not a speed result and must not be quoted as one."
  stop_port_forwards
  if [ "$FAILED" -ne 0 ]; then
    say "At least one check failed. Collect the evidence anyway: tools/eks-agent-run/50-collect.sh"
    return 6
  fi
  say "Every check passed. Next: tools/eks-agent-run/50-collect.sh, then 90-delete.sh --yes"
}

run_masked run_phase
