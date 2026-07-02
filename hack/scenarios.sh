#!/usr/bin/env bash
#
# End-to-end scenarios for llmfit-dra against a live cluster.
#   1. Publish  — the DaemonSet publishes a ResourceSlice for its node.
#   2. Shape    — expected devices exist with correct attributes/capacity.
#   3. Reconcile — a deleted slice is recreated by the helper controller.
#   4. Consume  — the scheduler allocates a ResourceClaim against our
#                 attributes via CEL (Phase 2 preview: the pod then sticks in
#                 ContainerCreating because there is deliberately no kubelet
#                 plugin yet — allocation succeeding is the assertion).
#
set -euo pipefail

DRIVER=llmfit.ai
NS=llmfit-dra
pass() { echo "  PASS: $1"; }
fail() { echo "  FAIL: $1" >&2; exit 1; }

slices_json() { kubectl get resourceslices -o json; }
our_slice() { slices_json | jq --arg d "$DRIVER" '[.items[] | select(.spec.driver == $d)]'; }

echo "== Scenario 1: publish"
kubectl -n "$NS" rollout status daemonset/llmfit-dra --timeout=120s >/dev/null
for i in $(seq 1 30); do
  count=$(our_slice | jq 'length')
  [ "$count" -ge 1 ] && break
  sleep 2
done
[ "$count" -ge 1 ] || fail "no ResourceSlice published by $DRIVER"
node=$(our_slice | jq -r '.[0].spec.nodeName')
[ -n "$node" ] && [ "$node" != "null" ] || fail "slice has no spec.nodeName"
pass "slice published for node '$node' (count=$count)"

echo "== Scenario 2: device shape"
devices=$(our_slice | jq '.[0].spec.devices')
names=$(echo "$devices" | jq -r '[.[].name] | sort | join(",")')
echo "  devices: $names"
echo "$devices" | jq -e '.[] | select(.name == "gpu0")' >/dev/null || fail "gpu0 missing"
echo "$devices" | jq -e '.[] | select(.name == "cpu0")' >/dev/null || fail "cpu0 missing"

gpu0=$(echo "$devices" | jq '.[] | select(.name == "gpu0")')
vendor=$(echo "$gpu0" | jq -r '.attributes.vendor.string')
case "$vendor" in intel|amd|nvidia) ;; *) fail "gpu0 vendor '$vendor' not a known vendor" ;; esac
[ "$(echo "$gpu0" | jq -r '.attributes.indexed.bool')" = "true" ] || fail "gpu0 not matched by capability index"
model=$(echo "$gpu0" | jq -r '.attributes.model.string')
bw=$(echo "$gpu0" | jq -r '.attributes.memoryBandwidthGBs.int')
[ "$bw" -gt 0 ] 2>/dev/null || fail "gpu0 has no memoryBandwidthGBs"
mem=$(echo "$gpu0" | jq -r '.capacity.memory.value // .capacity.memory')
[ -n "$mem" ] && [ "$mem" != "null" ] || fail "gpu0 has no memory capacity"
pass "gpu0: $vendor '$model', indexed, bandwidth=${bw}GB/s, memory=${mem}"

if echo "$devices" | jq -e '.[] | select(.name == "npu0")' >/dev/null; then
  pass "npu0 present ($(echo "$devices" | jq -r '.[] | select(.name=="npu0") | .attributes.model.string'))"
else
  echo "  SKIP: npu0 not present on this node"
fi

echo "== Scenario 3: reconcile after delete"
# Recreation is not instant: the upstream helper's mutation cache (60s TTL)
# can mask the delete on the first sync (30s SyncDelay), so worst case is
# roughly TTL + 2×SyncDelay ≈ 2 minutes. Allow 150s.
slice_name=$(our_slice | jq -r '.[0].metadata.name')
kubectl delete resourceslice "$slice_name" >/dev/null
recreated=""
for i in $(seq 1 75); do
  new_count=$(our_slice | jq 'length')
  new_name=$(our_slice | jq -r '.[0].metadata.name // empty')
  if [ "$new_count" -ge 1 ] && [ "$new_name" != "$slice_name" ]; then recreated="$new_name"; break; fi
  sleep 2
done
[ -n "$recreated" ] || fail "slice not recreated within 150s of deletion"
pass "slice recreated as '$recreated' after delete"

echo "== Scenario 4: scheduler consumes attributes via CEL"
cleanup() { kubectl delete --ignore-not-found pod/llmfit-consumer resourceclaim/llmfit-test-claim deviceclass/test.llmfit.ai >/dev/null 2>&1 || true; }
trap cleanup EXIT
cleanup
kubectl apply -f - <<'EOF' >/dev/null
apiVersion: resource.k8s.io/v1
kind: DeviceClass
metadata:
  name: test.llmfit.ai
spec:
  selectors:
    - cel:
        expression: device.driver == "llmfit.ai"
---
apiVersion: resource.k8s.io/v1
kind: ResourceClaim
metadata:
  name: llmfit-test-claim
spec:
  devices:
    requests:
      - name: accel
        exactly:
          deviceClassName: test.llmfit.ai
          selectors:
            - cel:
                expression: >-
                  device.attributes["llmfit.ai"].kind == "gpu" &&
                  device.attributes["llmfit.ai"].memoryBandwidthGBs >= 100 &&
                  device.capacity["llmfit.ai"].memory.compareTo(quantity("8Gi")) >= 0
---
apiVersion: v1
kind: Pod
metadata:
  name: llmfit-consumer
spec:
  restartPolicy: Never
  containers:
    - name: main
      image: registry.k8s.io/pause:3.10
      resources:
        claims:
          - name: accel
  resourceClaims:
    - name: accel
      resourceClaimName: llmfit-test-claim
EOF

allocated=""
for i in $(seq 1 30); do
  alloc_dev=$(kubectl get resourceclaim llmfit-test-claim -o jsonpath='{.status.allocation.devices.results[0].device}' 2>/dev/null || true)
  if [ -n "$alloc_dev" ]; then allocated="$alloc_dev"; break; fi
  sleep 2
done
[ -n "$allocated" ] || fail "scheduler did not allocate the claim (CEL over our attributes)"
[ "$allocated" = "gpu0" ] || fail "expected gpu0 to satisfy the CEL fit, got '$allocated'"
pass "kube-scheduler allocated '$allocated' via CEL over llmfit.ai attributes"
echo "  NOTE: pod remains Pending/ContainerCreating by design — no kubelet plugin in Phase 1."

echo "== Scenario 5: llmfit is the capability source"
gpu0=$(our_slice | jq '.[0].spec.devices[] | select(.name == "gpu0")')
src=$(echo "$gpu0" | jq -r '.attributes.source.string')
[ "$src" = "llmfit" ] || fail "gpu0 source is '$src', expected 'llmfit' (did llmfit exec fail in the pod?)"
backend=$(echo "$gpu0" | jq -r '.attributes.backend.string')
[ -n "$backend" ] && [ "$backend" != "null" ] || fail "gpu0 has no llmfit backend attribute"
model=$(echo "$gpu0" | jq -r '.attributes.model.string')
bw=$(echo "$gpu0" | jq -r '.attributes.memoryBandwidthGBs.int')
[ "$bw" -gt 0 ] 2>/dev/null || fail "gpu0 has no llmfit-sourced bandwidth"
cpu_src=$(our_slice | jq -r '.[0].spec.devices[] | select(.name == "cpu0") | .attributes.source.string')
[ "$cpu_src" = "llmfit" ] || fail "cpu0 source is '$cpu_src', expected 'llmfit'"
pass "llmfit assessed: '$model' via $backend, ${bw}GB/s (cpu0 also llmfit-sourced)"

echo
echo "All scenarios passed."
