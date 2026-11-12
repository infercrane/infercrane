#!/bin/sh
# Development fake for the local PoC only. Advertises an invented extended
# resource and product label on a CPU node so InferCrane's Kubernetes provider,
# which always requests an accelerator resource and selects on a product
# label, can schedule pods on a kind cluster. Nothing about the node changes:
# work still runs on the CPU, and the names say so.
#
#   simulated-accelerator.sh CONTEXT NODE RESOURCE LABEL_KEY LABEL_VALUE COUNT
#
# This is the "advertise extended resources for a node" pattern from the
# Kubernetes documentation. Never use it on a real cluster.
set -eu

if [ $# -ne 6 ]; then
  echo "usage: $0 CONTEXT NODE RESOURCE LABEL_KEY LABEL_VALUE COUNT" >&2
  exit 2
fi
context=$1 node=$2 resource=$3 label_key=$4 label_value=$5 count=$6

case $resource in
  *simulated*) ;;
  *) echo "refusing to advertise $resource: the simulated resource name must contain 'simulated'" >&2; exit 2 ;;
esac
case $label_value in
  *SIMULATED*) ;;
  *) echo "refusing label value $label_value: it must contain 'SIMULATED' so nobody mistakes it for a GPU" >&2; exit 2 ;;
esac

escaped=$(printf '%s' "$resource" | sed 's#~#~0#g; s#/#~1#g')
kubectl --context "$context" label node "$node" "$label_key=$label_value" --overwrite >/dev/null
kubectl --context "$context" patch node "$node" --subresource=status --type=json \
  -p "[{\"op\":\"add\",\"path\":\"/status/capacity/$escaped\",\"value\":\"$count\"},{\"op\":\"add\",\"path\":\"/status/allocatable/$escaped\",\"value\":\"$count\"}]" >/dev/null
# The kubelet recomputes allocatable from capacity on its next status update.
i=0
until kubectl --context "$context" get node "$node" -o "jsonpath={.status.allocatable}" | grep -q simulated; do
  i=$((i + 1))
  if [ $i -gt 30 ]; then
    echo "node $node did not report the simulated resource as allocatable" >&2
    exit 1
  fi
  sleep 1
done
echo "node $node advertises $count x $resource (simulated, CPU only) with $label_key=$label_value"
