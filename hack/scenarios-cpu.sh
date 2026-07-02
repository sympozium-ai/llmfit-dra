#!/usr/bin/env bash
#
# Run the scenario suite in CPU-only mode by hiding sysfs from the probe
# (--sys-root=/no-sys): the inventory collapses to cpu0, which is exactly
# what CI's GPU-less kind cluster sees. Lets any dev rig reproduce the CI
# run. The DaemonSet's original args are restored on exit.
set -euo pipefail

NS=llmfit-dra

orig_args=$(kubectl -n "$NS" get daemonset llmfit-dra -o json | jq -c '.spec.template.spec.containers[0].args')

set_args() {
  kubectl -n "$NS" patch daemonset llmfit-dra --type=json \
    -p="[{\"op\":\"replace\",\"path\":\"/spec/template/spec/containers/0/args\",\"value\":$1}]" >/dev/null
  kubectl -n "$NS" rollout status daemonset/llmfit-dra --timeout=120s >/dev/null
}

# wait_for_devices <jq-bool-expr over sorted device-name array>
wait_for_devices() {
  for i in $(seq 1 30); do
    names=$(kubectl get resourceslices -o json \
      | jq -c '[.items[] | select(.spec.driver == "llmfit.ai") | .spec.devices[].name] | sort')
    echo "$names" | jq -e "$1" >/dev/null 2>&1 && return 0
    sleep 2
  done
  echo "timed out waiting for inventory: last saw $names" >&2
  return 1
}

restore() {
  echo "== restoring original DaemonSet args"
  set_args "$orig_args"
}
trap restore EXIT

echo "== switching probe to CPU-only (--sys-root=/no-sys)"
set_args "$(echo "$orig_args" | jq -c '. + ["--sys-root=/no-sys"]')"
wait_for_devices '. == ["cpu0"]'

./hack/scenarios.sh
