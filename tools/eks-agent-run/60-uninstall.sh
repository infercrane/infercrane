#!/usr/bin/env bash
# Phase 6. Step 7 of the journey: the customer removes InferCrane from their
# cluster, and nothing of it is left. In this order:
#
#   1. every InferCrane deployment (`infercrane delete`), and the campaign
#      (`infercrane optimize cancel`), so the provider removes what it made;
#   2. the agent: `helm uninstall`;
#   3. the control plane and the bootstrap of docs/integrations/kubernetes.mdx
#      (`kubectl delete -f deploy/kubernetes/base/…`, in reverse order);
#   4. a listing of every object left that InferCrane made, namespaced or
#      cluster-wide. The customer's own server, and the Secrets the customer
#      made by hand, are theirs and are listed apart.
#
# The cluster itself is deleted by 90-delete.sh. This phase creates nothing.
. "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

RUN_DIR="$STATE_DIR/run"

uninstall_phase() {
  need_tools aws kubectl helm curl python3
  need_cluster
  guard_phase uninstall 20
  trap stop_port_forwards EXIT
  mkdir -p "$RUN_DIR"

  step "1. InferCrane deployments and the campaign"
  if kc --namespace "$NAMESPACE_HOSTED" get deployment infercrane >/dev/null 2>&1; then
    open_control
    local campaign name
    campaign=$(state_get MIG_CAMPAIGN)
    if [ -n "$campaign" ]; then
      icli optimize cancel "$campaign" --yes >"$RUN_DIR/cancel.txt" 2>&1 && say "  campaign $campaign cancelled" ||
        say "  cancel: $(tail -c 200 "$RUN_DIR/cancel.txt")"
    fi
    api GET /api/v1/deployments "$RUN_DIR/deployments-before-uninstall.json" >/dev/null || true
    for name in $(python3 -c 'import json, sys
try:
    d = json.load(open(sys.argv[1]))
except ValueError:
    d = {}
for item in d.get("deployments", d if isinstance(d, list) else []):
    if item.get("state") not in ("deleted",):
        print(item.get("name", ""))' "$RUN_DIR/deployments-before-uninstall.json"); do
      icli delete "$name" --yes --wait >>"$RUN_DIR/uninstall-deletes.txt" 2>&1 && say "  deleted $name" || say "  delete $name: $(tail -c 200 "$RUN_DIR/uninstall-deletes.txt")"
    done
    local left="" until=$((SECONDS + 300))
    while [ "$SECONDS" -lt "$until" ]; do
      left=$(kc get deployment,service,networkpolicy,pods --all-namespaces -l app.kubernetes.io/managed-by=infercrane -o name)
      [ -z "$left" ] && break
      sleep 10
    done
    [ -z "$left" ] && check provider_objects_gone pass "no Deployment, Service, NetworkPolicy or pod made by the provider or the executor is left" ||
      check provider_objects_gone fail "left: $(printf '%s' "$left" | tr '\n' ' ')"
    stop_port_forwards
  else
    say "  the control plane is gone already"
  fi

  step "2. helm uninstall $HELM_RELEASE --namespace $NAMESPACE_MODELS"
  if helm status "$HELM_RELEASE" --namespace "$NAMESPACE_MODELS" >/dev/null 2>&1; then
    helm uninstall "$HELM_RELEASE" --namespace "$NAMESPACE_MODELS" --wait --timeout 3m | sed 's/^/  /'
  else
    say "  no release"
  fi
  local agent_left
  agent_left=$(kc --namespace "$NAMESPACE_MODELS" get all,role,rolebinding,serviceaccount,secret,configmap,networkpolicy -o name |
    grep -i -E 'infercrane|agent' | grep -v -E '^secret/(infercrane-agent-key|model-server-token)$' || true)
  printf '%s\n' "$agent_left" >"$RUN_DIR/agent-left.txt"
  case "$agent_left" in
    '') check helm_uninstall_clean pass "nothing of the release is left in $NAMESPACE_MODELS" ;;
    "secret/$HELM_RELEASE-infercrane-agent-hash-key")
      check helm_uninstall_clean pass "only the hashing key Secret is left, kept on purpose so request hashes stay comparable (docs/architecture/agent.mdx); delete it by hand to remove everything" ;;
    *) check helm_uninstall_clean fail "left in $NAMESPACE_MODELS: $(printf '%s' "$agent_left" | tr '\n' ' ')" ;;
  esac
  say "  made by hand by the customer, not by InferCrane: $(kc --namespace "$NAMESPACE_MODELS" get secret infercrane-agent-key model-server-token -o name 2>/dev/null | tr '\n' ' ')"

  step "3. The control plane and the bootstrap"
  render "$TOOL_DIR/manifests/hosted.yaml" | kc delete --ignore-not-found --wait=true -f - | sed 's/^/  /' || true
  kc --namespace "$NAMESPACE_HOSTED" delete secret control-plane control-plane-tls infercrane-worker --ignore-not-found | sed 's/^/  /'
  local base
  for base in provider-rbac service-accounts namespace; do
    kc delete --ignore-not-found --wait=true --timeout=180s -f "$REPO_DIR/deploy/kubernetes/base/$base.yaml" | sed 's/^/  /' || true
  done

  step "4. What InferCrane left in the cluster"
  local leftovers
  leftovers=$( {
    kc get namespace "$NAMESPACE_HOSTED" -o name 2>/dev/null || true
    kc get all,role,rolebinding,serviceaccount,secret,configmap,networkpolicy,pvc --all-namespaces -o name -l app.kubernetes.io/managed-by=infercrane 2>/dev/null || true
    kc get clusterrole,clusterrolebinding,customresourcedefinition,validatingwebhookconfiguration,mutatingwebhookconfiguration,priorityclass -o name 2>/dev/null | grep -i infercrane || true
    kc get all,role,rolebinding,serviceaccount,secret,configmap,networkpolicy --all-namespaces -o name 2>/dev/null | grep -i infercrane |
      grep -v -E '^secret/(infercrane-agent-key|agent-infercrane-agent-hash-key)$' || true
  } | sort -u | grep -v '^$' || true)
  printf '%s\n' "$leftovers" >"$RUN_DIR/leftovers.txt"
  [ -z "$leftovers" ] && check uninstall_clean pass "no namespace, workload, policy, role, binding, account, Secret, ConfigMap, CRD or webhook of InferCrane is left" ||
    check uninstall_clean fail "left: $(tr '\n' ' ' <"$RUN_DIR/leftovers.txt")"
  kc get pods --all-namespaces -o wide >"$RUN_DIR/pods-after-uninstall.txt"
  say "  pods still running (the cluster's own and the customer's): $(awk 'NR > 1 {print $1}' "$RUN_DIR/pods-after-uninstall.txt" | sort | uniq -c | tr '\n' ' ')"

  echo
  say "Next: tools/eks-agent-run/50-collect.sh, then tools/eks-agent-run/90-delete.sh --yes"
}

run_masked uninstall_phase
