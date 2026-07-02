#!/usr/bin/env bash
#
# End-to-end scenarios for llmfit-dra against a live cluster.
#   1. Publish  — the DaemonSet publishes a ResourceSlice for its node.
#   2. Shape    — expected devices exist with correct attributes/capacity.
#   3. Reconcile — a deleted slice is recreated by the helper controller.
#   4. Consume  — the shipped DeviceClasses exist, the scheduler allocates
#                 a ResourceClaim against our attributes via CEL, and the
#                 kubelet plugin prepares it: the pod reaches Running with
#                 the CDI edits (LLMFIT_* env + device nodes) visible inside.
#   6. CPU path — a cpu.llmfit.ai claim prepares env-only and runs on any
#                 node, accelerator or not (the kind-friendly e2e).
#   7. Lifecycle — the per-claim CDI spec exists while the pod runs and is
#                 removed by unprepare when the pod is deleted.
#   8. Restart  — driver restart: running consumers survive, the new
#                 instance prepares fresh claims and unprepares claims the
#                 old instance prepared (CDI file is the only state).
#   9. Claim gen — `llmfit claim <model>` (run inside the driver image)
#                 emits a ResourceClaim that applies, allocates, and runs.
#
set -euo pipefail

DRIVER=llmfit.ai
NS=llmfit-dra
pass() { echo "  PASS: $1"; }
fail() { echo "  FAIL: $1" >&2; exit 1; }

slices_json() { kubectl get resourceslices -o json; }
our_slice() { slices_json | jq --arg d "$DRIVER" '[.items[] | select(.spec.driver == $d)]'; }

# Exec inside the (Running) driver pod — it mounts host /var/run/cdi, so it
# can observe the kubelet plugin's on-disk prepare state.
driver_exec() {
  local pod
  pod=$(kubectl -n "$NS" get pod --field-selector=status.phase=Running -o name | head -1)
  kubectl -n "$NS" exec "${pod#pod/}" -- "$@"
}

# apply_consumer <pod> <claim> <deviceclass>: a claim with no extra CEL (the
# class is the whole selector) plus a pod consuming it.
apply_consumer() {
  kubectl apply -f - <<EOF >/dev/null
apiVersion: resource.k8s.io/v1
kind: ResourceClaim
metadata:
  name: $2
spec:
  devices:
    requests:
      - name: dev
        exactly:
          deviceClassName: $3
---
apiVersion: v1
kind: Pod
metadata:
  name: $1
spec:
  restartPolicy: Never
  terminationGracePeriodSeconds: 1
  containers:
    - name: main
      image: docker.io/library/busybox:1.36
      command: ["sleep", "600"]
      resources:
        claims:
          - name: dev
  resourceClaims:
    - name: dev
      resourceClaimName: $2
EOF
}

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
echo "$devices" | jq -e '.[] | select(.name == "cpu0")' >/dev/null || fail "cpu0 missing"
cpu_mem=$(echo "$devices" | jq -r '.[] | select(.name == "cpu0") | .capacity.memory.value // .capacity.memory')
[ -n "$cpu_mem" ] && [ "$cpu_mem" != "null" ] || fail "cpu0 has no memory capacity"

# GPU assertions run only where a GPU exists — the same suite must pass on
# a GPU-less CI kind cluster via the cpu0 fallback device.
HAS_GPU=0
if echo "$devices" | jq -e '.[] | select(.name == "gpu0")' >/dev/null; then
  HAS_GPU=1
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
else
  echo "  SKIP: no gpu0 on this node — running in CPU-only mode"
  pass "cpu0 present with memory capacity ${cpu_mem}"
fi

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
# Uses the SHIPPED DeviceClasses (deploy/deviceclass.yaml, applied by
# `make deploy`): gpu.llmfit.ai owns the kind check, the claim adds only
# the fit physics — the layering `llmfit claim` will rely on.
for class in llmfit.ai gpu.llmfit.ai npu.llmfit.ai cpu.llmfit.ai; do
  kubectl get deviceclass "$class" >/dev/null 2>&1 || fail "shipped DeviceClass '$class' missing (run 'make deploy')"
done
pass "shipped DeviceClasses present"
cleanup() { kubectl delete --ignore-not-found pod/llmfit-consumer resourceclaim/llmfit-test-claim >/dev/null 2>&1 || true; }
trap cleanup EXIT
cleanup
if [ "$HAS_GPU" = 1 ]; then
  CLASS=gpu.llmfit.ai WANT=gpu0
  CEL='device.attributes["llmfit.ai"].memoryBandwidthGBs >= 100 &&
                  device.capacity["llmfit.ai"].memory.compareTo(quantity("8Gi")) >= 0'
else
  CLASS=cpu.llmfit.ai WANT=cpu0
  CEL='device.capacity["llmfit.ai"].memory.compareTo(quantity("1Gi")) >= 0'
fi
kubectl apply -f - <<EOF >/dev/null
apiVersion: resource.k8s.io/v1
kind: ResourceClaim
metadata:
  name: llmfit-test-claim
spec:
  devices:
    requests:
      - name: accel
        exactly:
          deviceClassName: $CLASS
          selectors:
            - cel:
                expression: >-
                  $CEL
---
apiVersion: v1
kind: Pod
metadata:
  name: llmfit-consumer
spec:
  restartPolicy: Never
  terminationGracePeriodSeconds: 1
  containers:
    - name: main
      image: docker.io/library/busybox:1.36
      command: ["sleep", "600"]
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
[ "$allocated" = "$WANT" ] || fail "expected $WANT to satisfy the CEL fit, got '$allocated'"
pass "kube-scheduler allocated '$allocated' via CEL over llmfit.ai attributes"

# Phase 2: the kubelet plugin prepares the claim (CDI), so the pod must RUN.
kubectl wait --for=condition=Ready pod/llmfit-consumer --timeout=120s >/dev/null \
  || fail "pod did not reach Running — kubelet plugin prepare failed? (kubectl describe pod llmfit-consumer)"
env_dev=$(kubectl exec llmfit-consumer -- sh -c 'echo $LLMFIT_DEVICE')
[ "$env_dev" = "$WANT" ] || fail "LLMFIT_DEVICE in container is '$env_dev', want $WANT"
if [ "$HAS_GPU" = 1 ]; then
  render=$(kubectl exec llmfit-consumer -- sh -c 'echo $LLMFIT_RENDER_NODE')
  [ -n "$render" ] || fail "LLMFIT_RENDER_NODE not set in container"
  kubectl exec llmfit-consumer -- test -e "$render" || fail "render node $render not injected into container"
  pass "pod Running with CDI edits: LLMFIT_DEVICE=$env_dev, $render present in /dev"
else
  pass "pod Running with CDI edits: LLMFIT_DEVICE=$env_dev (env-only, no GPU)"
fi

echo "== Scenario 5: llmfit is the capability source"
cpu_src=$(our_slice | jq -r '.[0].spec.devices[] | select(.name == "cpu0") | .attributes.source.string')
[ "$cpu_src" = "llmfit" ] || fail "cpu0 source is '$cpu_src', expected 'llmfit' (did llmfit exec fail in the pod?)"
if [ "$HAS_GPU" = 1 ]; then
  gpu0=$(our_slice | jq '.[0].spec.devices[] | select(.name == "gpu0")')
  src=$(echo "$gpu0" | jq -r '.attributes.source.string')
  [ "$src" = "llmfit" ] || fail "gpu0 source is '$src', expected 'llmfit'"
  backend=$(echo "$gpu0" | jq -r '.attributes.backend.string')
  [ -n "$backend" ] && [ "$backend" != "null" ] || fail "gpu0 has no llmfit backend attribute"
  model=$(echo "$gpu0" | jq -r '.attributes.model.string')
  bw=$(echo "$gpu0" | jq -r '.attributes.memoryBandwidthGBs.int')
  [ "$bw" -gt 0 ] 2>/dev/null || fail "gpu0 has no llmfit-sourced bandwidth"
  pass "llmfit assessed: '$model' via $backend, ${bw}GB/s (cpu0 also llmfit-sourced)"
else
  pass "llmfit assessed cpu0 (no GPU on this node)"
fi

echo "== Scenario 6: cpu0 claim is env-only (runs anywhere, no accelerator needed)"
# cpu0 is exclusive; in CPU-only mode scenario 4's consumer holds it.
cleanup
kubectl wait --for=delete pod/llmfit-consumer --timeout=60s >/dev/null 2>&1 || true
cleanup6() { kubectl delete --ignore-not-found pod/llmfit-cpu-consumer resourceclaim/llmfit-cpu-claim >/dev/null 2>&1 || true; }
trap 'cleanup; cleanup6' EXIT
cleanup6
kubectl apply -f - <<'EOF' >/dev/null
apiVersion: resource.k8s.io/v1
kind: ResourceClaim
metadata:
  name: llmfit-cpu-claim
spec:
  devices:
    requests:
      - name: fallback
        exactly:
          deviceClassName: cpu.llmfit.ai
---
apiVersion: v1
kind: Pod
metadata:
  name: llmfit-cpu-consumer
spec:
  restartPolicy: Never
  terminationGracePeriodSeconds: 1
  containers:
    - name: main
      image: docker.io/library/busybox:1.36
      command: ["sleep", "600"]
      resources:
        claims:
          - name: fallback
  resourceClaims:
    - name: fallback
      resourceClaimName: llmfit-cpu-claim
EOF
kubectl wait --for=condition=Ready pod/llmfit-cpu-consumer --timeout=120s >/dev/null \
  || fail "cpu-claim pod did not reach Running"
env_dev=$(kubectl exec llmfit-cpu-consumer -- sh -c 'echo $LLMFIT_DEVICE')
[ "$env_dev" = "cpu0" ] || fail "LLMFIT_DEVICE is '$env_dev', want cpu0"
kubectl exec llmfit-cpu-consumer -- sh -c 'test ! -e /dev/dri' || fail "cpu0 claim must not inject GPU device nodes"
pass "cpu0 claim prepared env-only: LLMFIT_DEVICE=$env_dev, no device nodes injected"

echo "== Scenario 7: unprepare removes CDI state when the pod goes away"
# Devices are exclusive: release the earlier consumers so their devices can
# be re-claimed below.
cleanup; cleanup6
kubectl wait --for=delete pod/llmfit-consumer pod/llmfit-cpu-consumer --timeout=60s >/dev/null 2>&1 || true
cleanup7() { kubectl delete --ignore-not-found --grace-period=1 pod/llmfit-lifecycle >/dev/null 2>&1; kubectl delete --ignore-not-found resourceclaim/llmfit-lifecycle-claim >/dev/null 2>&1; }
trap 'cleanup; cleanup6; cleanup7' EXIT
apply_consumer llmfit-lifecycle llmfit-lifecycle-claim cpu.llmfit.ai
kubectl wait --for=condition=Ready pod/llmfit-lifecycle --timeout=120s >/dev/null || fail "lifecycle pod not Running"
uid=$(kubectl get resourceclaim llmfit-lifecycle-claim -o jsonpath='{.metadata.uid}')
driver_exec test -f "/var/run/cdi/llmfit.ai-$uid.json" || fail "CDI spec for claim $uid missing while pod is Running"
kubectl delete pod llmfit-lifecycle --grace-period=1 --wait >/dev/null
gone=""
for i in $(seq 1 30); do
  if ! driver_exec test -e "/var/run/cdi/llmfit.ai-$uid.json" 2>/dev/null; then gone=1; break; fi
  sleep 2
done
[ -n "$gone" ] || fail "CDI spec llmfit.ai-$uid.json still on the node after pod deletion (unprepare not called or failed)"
kubectl delete resourceclaim llmfit-lifecycle-claim >/dev/null
pass "CDI spec existed while Running, removed on unprepare"

echo "== Scenario 8: driver restart is seamless (running pods survive, prepare/unprepare still work)"
cleanup8() { kubectl delete --ignore-not-found --grace-period=1 pod/llmfit-survivor pod/llmfit-post-restart >/dev/null 2>&1; kubectl delete --ignore-not-found resourceclaim/llmfit-survivor-claim resourceclaim/llmfit-post-restart-claim >/dev/null 2>&1; }
trap 'cleanup; cleanup6; cleanup7; cleanup8' EXIT
apply_consumer llmfit-survivor llmfit-survivor-claim cpu.llmfit.ai
kubectl wait --for=condition=Ready pod/llmfit-survivor --timeout=120s >/dev/null || fail "survivor pod not Running before restart"
survivor_uid=$(kubectl get resourceclaim llmfit-survivor-claim -o jsonpath='{.metadata.uid}')

kubectl -n "$NS" rollout restart daemonset/llmfit-dra >/dev/null
kubectl -n "$NS" rollout status daemonset/llmfit-dra --timeout=120s >/dev/null || fail "driver did not come back after restart"

phase=$(kubectl get pod llmfit-survivor -o jsonpath='{.status.phase}')
[ "$phase" = "Running" ] || fail "survivor pod is '$phase' after driver restart, want Running"

# The new instance must unprepare claims the PREVIOUS instance prepared
# (state is the CDI file, not process memory)…
kubectl delete pod llmfit-survivor --grace-period=1 --wait >/dev/null
gone=""
for i in $(seq 1 30); do
  if ! driver_exec test -e "/var/run/cdi/llmfit.ai-$survivor_uid.json" 2>/dev/null; then gone=1; break; fi
  sleep 2
done
[ -n "$gone" ] || fail "pre-restart claim's CDI spec not cleaned up by the new instance"
# …and serve fresh prepares (re-registration). Sequential so the suite needs
# only one allocatable device (cpu0) — CI kind nodes have no GPU.
kubectl delete resourceclaim llmfit-survivor-claim >/dev/null
apply_consumer llmfit-post-restart llmfit-post-restart-claim cpu.llmfit.ai
kubectl wait --for=condition=Ready pod/llmfit-post-restart --timeout=120s >/dev/null \
  || fail "post-restart claim did not prepare (plugin re-registration broken?)"
pass "restart seamless: survivor stayed Running, cross-restart unprepare cleaned up, new claim prepared"

echo "== Scenario 9: llmfit claim generates a working ResourceClaim"
# The generator inlines model-DB constants into fit CEL; the bandwidth floor
# only ever matches accelerator devices, so this needs a GPU.
if [ "$HAS_GPU" = 1 ]; then
  cleanup9() { kubectl delete --ignore-not-found --grace-period=1 pod/llmfit-claim-consumer >/dev/null 2>&1; kubectl delete --ignore-not-found resourceclaim/qwen-qwen2-5-7b-fit >/dev/null 2>&1; }
  trap 'cleanup; cleanup6; cleanup7; cleanup8; cleanup9' EXIT
  cleanup9
  driver_exec llmfit claim Qwen/Qwen2.5-7B --min-tps 20 | kubectl apply -f - >/dev/null \
    || fail "llmfit claim output did not apply cleanly"
  kubectl apply -f - <<'EOF' >/dev/null
apiVersion: v1
kind: Pod
metadata:
  name: llmfit-claim-consumer
spec:
  restartPolicy: Never
  terminationGracePeriodSeconds: 1
  containers:
    - name: main
      image: docker.io/library/busybox:1.36
      command: ["sleep", "600"]
      resources:
        claims:
          - name: model
  resourceClaims:
    - name: model
      resourceClaimName: qwen-qwen2-5-7b-fit
EOF
  kubectl wait --for=condition=Ready pod/llmfit-claim-consumer --timeout=120s >/dev/null \
    || fail "pod with generated claim did not reach Running"
  alloc=$(kubectl get resourceclaim qwen-qwen2-5-7b-fit -o jsonpath='{.status.allocation.devices.results[0].device}')
  env_dev=$(kubectl exec llmfit-claim-consumer -- sh -c 'echo $LLMFIT_DEVICE')
  [ -n "$alloc" ] && [ "$env_dev" = "$alloc" ] || fail "allocated '$alloc' but container sees LLMFIT_DEVICE='$env_dev'"
  pass "generated claim allocated '$alloc' and pod is Running with matching CDI env"
else
  echo "  SKIP: needs an accelerator (generated CEL has a bandwidth floor)"
fi

echo
echo "All scenarios passed."
