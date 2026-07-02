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
#  10. Deployment — the Phase 2 exit criterion verbatim: a vanilla
#                 Deployment + generated ResourceClaimTemplate lands on the
#                 fit device and runs; the per-pod claim is GC'd on delete.
#  11. Align    — Phase 3: one claim requests gpu + npu constrained by
#                 matchAttribute resource.kubernetes.io/pcieRoot (the
#                 standardized cross-driver attribute); both allocate, both
#                 prepare into one pod (multi-device CDI merge).
#  12. Hotplug  — Phase 3: a synthesized kernel uevent (echo change >
#                 …/uevent on the host) triggers an immediate re-probe
#                 instead of waiting out the ticker.
#  13. Coexist  — Phase 4: a (forged) vendor DRA driver slice for this node
#                 demotes our GPUs to fitness-only (vendorManaged attr);
#                 default classes refuse them; removal restores allocation.
#  14. Admission — Phase 4: the ValidatingAdmissionPolicy denies the driver
#                 SA writing slices for other nodes / other drivers, while
#                 the driver's own node-bound writes keep working.
#
set -euo pipefail

DRIVER=llmfit.ai
NS=llmfit-dra
pass() { echo "  PASS: $1"; }
fail() { echo "  FAIL: $1" >&2; exit 1; }

slices_json() { kubectl get resourceslices -o json; }
our_slice() { slices_json | jq --arg d "$DRIVER" '[.items[] | select(.spec.driver == $d)]'; }

# Exec inside a (Running) driver pod — it mounts host /var/run/cdi, so it
# can observe the kubelet plugin's on-disk prepare state. driver_exec_on
# targets the pod on a SPECIFIC node: on multi-node clusters, prepare state
# lives only on the node where the consumer landed.
driver_exec_on() {
  local target=$1 pod
  shift
  pod=$(kubectl -n "$NS" get pod --field-selector="spec.nodeName=$target,status.phase=Running" -o name | head -1)
  kubectl -n "$NS" exec "${pod#pod/}" -- "$@"
}
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

# --- Cleanup: every function is defined UNCONDITIONALLY (scenarios that
# SKIP still leave the EXIT trap valid) and idempotent (--ignore-not-found),
# so one trap covers every path including early fail().
cleanup()   { kubectl delete --ignore-not-found pod/llmfit-consumer resourceclaim/llmfit-test-claim >/dev/null 2>&1 || true; }
cleanup6()  { kubectl delete --ignore-not-found pod/llmfit-cpu-consumer resourceclaim/llmfit-cpu-claim >/dev/null 2>&1 || true; }
cleanup7()  { kubectl delete --ignore-not-found --grace-period=1 pod/llmfit-lifecycle >/dev/null 2>&1; kubectl delete --ignore-not-found resourceclaim/llmfit-lifecycle-claim >/dev/null 2>&1; }
cleanup8()  { kubectl delete --ignore-not-found --grace-period=1 pod/llmfit-survivor pod/llmfit-post-restart >/dev/null 2>&1; kubectl delete --ignore-not-found resourceclaim/llmfit-survivor-claim resourceclaim/llmfit-post-restart-claim >/dev/null 2>&1; }
cleanup9()  { kubectl delete --ignore-not-found --grace-period=1 pod/llmfit-claim-consumer >/dev/null 2>&1; kubectl delete --ignore-not-found resourceclaim/qwen-qwen2-5-7b-fit >/dev/null 2>&1; }
cleanup10() { kubectl delete --ignore-not-found --grace-period=1 deployment/llmfit-deploy-consumer >/dev/null 2>&1; kubectl delete --ignore-not-found resourceclaimtemplate/qwen-fit-tmpl >/dev/null 2>&1; }
cleanup11() { kubectl delete --ignore-not-found --grace-period=1 pod/llmfit-aligned-consumer >/dev/null 2>&1; kubectl delete --ignore-not-found resourceclaim/llmfit-aligned-claim >/dev/null 2>&1; }
cleanup13() { kubectl delete --ignore-not-found resourceslice/fake-vendor-slice pod/llmfit-coexist-consumer resourceclaim/llmfit-coexist-claim >/dev/null 2>&1; }
cleanup15() { kubectl delete --ignore-not-found --grace-period=1 pod/llmfit-compute >/dev/null 2>&1; kubectl delete --ignore-not-found resourceclaim/llmfit-compute-claim >/dev/null 2>&1; }
cleanup_host() { kubectl -n kube-system delete pod scenario-host --ignore-not-found --grace-period=1 >/dev/null 2>&1; }
trap 'cleanup; cleanup6; cleanup7; cleanup8; cleanup9; cleanup10; cleanup11; cleanup13; cleanup15; cleanup_host' EXIT

echo "== Scenario 1: publish"
kubectl -n "$NS" rollout status daemonset/llmfit-dra --timeout=120s >/dev/null
nodes_total=$(kubectl get nodes --no-headers | wc -l)
for i in $(seq 1 30); do
  count=$(our_slice | jq 'length')
  [ "$count" -ge "$nodes_total" ] && break
  sleep 2
done
# Multi-node honesty: EVERY node must publish, not just one.
[ "$count" -ge "$nodes_total" ] || fail "only $count/$nodes_total nodes published a ResourceSlice"
slice_nodes=$(our_slice | jq -r '[.[].spec.nodeName] | unique | length')
[ "$slice_nodes" = "$nodes_total" ] || fail "slices cover $slice_nodes/$nodes_total nodes"
node=$(our_slice | jq -r '.[0].spec.nodeName')
pass "slices published for all $nodes_total node(s)"

echo "== Scenario 2: device shape"
devices=$(our_slice | jq '.[0].spec.devices')
names=$(echo "$devices" | jq -r '[.[].name] | sort | join(",")')
echo "  devices: $names"
echo "$devices" | jq -e '.[] | select(.name == "cpu0")' >/dev/null || fail "cpu0 missing"
cpu_mem=$(echo "$devices" | jq -r '.[] | select(.name == "cpu0") | .capacity.memory.value // .capacity.memory')
[ -n "$cpu_mem" ] && [ "$cpu_mem" != "null" ] || fail "cpu0 has no memory capacity"

# GPU assertions run only where a FIT-CAPABLE gpu exists (one the index or
# llmfit priced — virtual display adapters on CI runners publish as gpu0
# with no bandwidth and must not count). The same suite must pass on such
# nodes via the cpu0 fallback device.
HAS_GPU=0
GPU_DEV=$(echo "$devices" | jq -r 'first(.[] | select(.attributes.kind.string == "gpu" and (.attributes.memoryBandwidthGBs.int // 0) > 0)) | .name // empty')
if [ -n "$GPU_DEV" ]; then
  HAS_GPU=1
  gpu0=$(echo "$devices" | jq --arg n "$GPU_DEV" '.[] | select(.name == $n)')
  vendor=$(echo "$gpu0" | jq -r '.attributes.vendor.string')
  case "$vendor" in intel|amd|nvidia) ;; *) fail "gpu0 vendor '$vendor' not a known vendor" ;; esac
  [ "$(echo "$gpu0" | jq -r '.attributes.indexed.bool')" = "true" ] || fail "gpu0 not matched by capability index"
  model=$(echo "$gpu0" | jq -r '.attributes.model.string')
  bw=$(echo "$gpu0" | jq -r '.attributes.memoryBandwidthGBs.int')
  [ "$bw" -gt 0 ] 2>/dev/null || fail "gpu0 has no memoryBandwidthGBs"
  mem=$(echo "$gpu0" | jq -r '.capacity.memory.value // .capacity.memory')
  [ -n "$mem" ] && [ "$mem" != "null" ] || fail "gpu0 has no memory capacity"
  pass "$GPU_DEV: $vendor '$model', indexed, bandwidth=${bw}GB/s, memory=${mem}"
else
  echo "  SKIP: no fit-capable gpu0 on this node — running in CPU-only mode"
  pass "cpu0 present with memory capacity ${cpu_mem}"
fi

NPU_DEV=$(echo "$devices" | jq -r 'first(.[] | select(.attributes.kind.string == "npu")) | .name // empty')
if [ -n "$NPU_DEV" ]; then
  pass "$NPU_DEV present ($(echo "$devices" | jq --arg n "$NPU_DEV" -r '.[] | select(.name==$n) | .attributes.model.string'))"
else
  echo "  SKIP: no npu on this node"
fi

echo "== Scenario 3: reconcile after delete"
# Recreation is not instant: the upstream helper's mutation cache (60s TTL)
# can mask the delete on the first sync (30s SyncDelay), so worst case is
# roughly TTL + 2×SyncDelay ≈ 2 minutes. Allow 150s.
slice_name=$(our_slice | jq -r '.[0].metadata.name')
slice_node=$(our_slice | jq -r '.[0].spec.nodeName')
kubectl delete resourceslice "$slice_name" >/dev/null
recreated=""
for i in $(seq 1 75); do
  # Multi-node: the recreated slice must be for the SAME node — another
  # node's untouched slice must not satisfy this.
  recreated=$(our_slice | jq -r --arg n "$slice_node" --arg old "$slice_name" \
    'first(.[] | select(.spec.nodeName == $n and .metadata.name != $old)) | .metadata.name // empty')
  [ -n "$recreated" ] && break
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
cleanup
if [ "$HAS_GPU" = 1 ]; then
  CLASS=gpu.llmfit.ai WANT=$GPU_DEV
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
  gpu0=$(our_slice | jq --arg n "$GPU_DEV" '.[0].spec.devices[] | select(.name == $n)')
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
# The capability must have arrived over the serve API (AF_UNIX sidecar),
# not the exec fallback — and the sidecar must be healthy.
driver_exec curl -sf --unix-socket /run/llmfit/llmfit.sock http://localhost/health >/dev/null \
  || fail "llmfit sidecar socket not serving /health"
kubectl -n "$NS" logs ds/llmfit-dra -c llmfit-dra | grep -q 'llmfit capability transport" transport="api"' \
  || fail "driver did not use the API transport (exec fallback or index in use?)"
pass "capability flows over the AF_UNIX serve API (sidecar healthy)"

echo "== Scenario 6: cpu0 claim is env-only (runs anywhere, no accelerator needed)"
# cpu0 is exclusive; in CPU-only mode scenario 4's consumer holds it.
cleanup
kubectl wait --for=delete pod/llmfit-consumer --timeout=60s >/dev/null 2>&1 || true
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
apply_consumer llmfit-lifecycle llmfit-lifecycle-claim cpu.llmfit.ai
kubectl wait --for=condition=Ready pod/llmfit-lifecycle --timeout=120s >/dev/null || fail "lifecycle pod not Running"
uid=$(kubectl get resourceclaim llmfit-lifecycle-claim -o jsonpath='{.metadata.uid}')
lnode=$(kubectl get pod llmfit-lifecycle -o jsonpath='{.spec.nodeName}')
driver_exec_on "$lnode" test -f "/var/run/cdi/llmfit.ai-$uid.json" || fail "CDI spec for claim $uid missing while pod is Running"
kubectl delete pod llmfit-lifecycle --grace-period=1 --wait >/dev/null
gone=""
for i in $(seq 1 30); do
  if ! driver_exec_on "$lnode" test -e "/var/run/cdi/llmfit.ai-$uid.json" 2>/dev/null; then gone=1; break; fi
  sleep 2
done
[ -n "$gone" ] || fail "CDI spec llmfit.ai-$uid.json still on the node after pod deletion (unprepare not called or failed)"
kubectl delete resourceclaim llmfit-lifecycle-claim >/dev/null
pass "CDI spec existed while Running, removed on unprepare"

echo "== Scenario 8: driver restart is seamless (running pods survive, prepare/unprepare still work)"
apply_consumer llmfit-survivor llmfit-survivor-claim cpu.llmfit.ai
kubectl wait --for=condition=Ready pod/llmfit-survivor --timeout=120s >/dev/null || fail "survivor pod not Running before restart"
survivor_uid=$(kubectl get resourceclaim llmfit-survivor-claim -o jsonpath='{.metadata.uid}')
snode=$(kubectl get pod llmfit-survivor -o jsonpath='{.spec.nodeName}')

kubectl -n "$NS" rollout restart daemonset/llmfit-dra >/dev/null
kubectl -n "$NS" rollout status daemonset/llmfit-dra --timeout=120s >/dev/null || fail "driver did not come back after restart"

phase=$(kubectl get pod llmfit-survivor -o jsonpath='{.status.phase}')
[ "$phase" = "Running" ] || fail "survivor pod is '$phase' after driver restart, want Running"

# The new instance must unprepare claims the PREVIOUS instance prepared
# (state is the CDI file, not process memory)…
kubectl delete pod llmfit-survivor --grace-period=1 --wait >/dev/null
gone=""
for i in $(seq 1 30); do
  if ! driver_exec_on "$snode" test -e "/var/run/cdi/llmfit.ai-$survivor_uid.json" 2>/dev/null; then gone=1; break; fi
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
  cleanup9
  claim_yaml=$(driver_exec llmfit claim Qwen/Qwen2.5-7B --min-tps 20)
  # Optional lookups must be guarded (missing attr = non-match, not error).
  echo "$claim_yaml" | grep -q "'memoryBandwidthGBs' in device.attributes" \
    || fail "generated CEL lacks the optional-attribute guard"
  echo "$claim_yaml" | kubectl apply -f - >/dev/null \
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

echo "== Scenario 10: Deployment + generated ResourceClaimTemplate (Phase 2 exit criterion)"
if [ "$HAS_GPU" = 1 ]; then
  # gpu0 is exclusive — release scenario 9's consumer first.
  cleanup9
  kubectl wait --for=delete pod/llmfit-claim-consumer --timeout=60s >/dev/null 2>&1 || true
  cleanup10
  driver_exec llmfit claim Qwen/Qwen2.5-7B --min-tps 20 --template --name qwen-fit-tmpl | kubectl apply -f - >/dev/null \
    || fail "llmfit claim --template output did not apply cleanly"
  kubectl apply -f - <<'EOF' >/dev/null
apiVersion: apps/v1
kind: Deployment
metadata:
  name: llmfit-deploy-consumer
spec:
  replicas: 1
  selector:
    matchLabels:
      app: llmfit-deploy-consumer
  template:
    metadata:
      labels:
        app: llmfit-deploy-consumer
    spec:
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
          resourceClaimTemplateName: qwen-fit-tmpl
EOF
  kubectl rollout status deployment/llmfit-deploy-consumer --timeout=120s >/dev/null \
    || fail "deployment did not become ready with a template-instantiated claim"
  dpod=$(kubectl get pod -l app=llmfit-deploy-consumer -o name | head -1)
  env_dev=$(kubectl exec "${dpod#pod/}" -- sh -c 'echo $LLMFIT_DEVICE')
  [ "$env_dev" = "$GPU_DEV" ] || fail "deployment pod sees LLMFIT_DEVICE='$env_dev', want $GPU_DEV"
  gen_claims=$(kubectl get resourceclaims -o json | jq '[.items[] | select(.metadata.annotations["resource.kubernetes.io/pod-claim-name"] == "model")] | length')
  [ "$gen_claims" -ge 1 ] || fail "no per-pod ResourceClaim instantiated from the template"
  kubectl delete deployment llmfit-deploy-consumer --wait >/dev/null
  gone=""
  for i in $(seq 1 30); do
    left=$(kubectl get resourceclaims -o json | jq '[.items[] | select(.metadata.annotations["resource.kubernetes.io/pod-claim-name"] == "model")] | length')
    [ "$left" = 0 ] && { gone=1; break; }
    sleep 2
  done
  [ -n "$gone" ] || fail "template-instantiated claim not garbage-collected after deployment delete"
  pass "Deployment ran on $GPU_DEV via generated template; per-pod claim GC'd on delete"
else
  echo "  SKIP: needs an accelerator (generated CEL has a bandwidth floor)"
fi

echo "== Scenario 11: matchAttribute alignment via resource.kubernetes.io/pcieRoot"
HAS_NPU=0
[ -n "${NPU_DEV:-}" ] && HAS_NPU=1
if [ "$HAS_GPU" = 1 ] && [ "$HAS_NPU" = 1 ]; then
  cleanup11
  kubectl apply -f - <<'EOF' >/dev/null
apiVersion: resource.k8s.io/v1
kind: ResourceClaim
metadata:
  name: llmfit-aligned-claim
spec:
  devices:
    requests:
      - name: gpu
        exactly:
          deviceClassName: gpu.llmfit.ai
      - name: npu
        exactly:
          deviceClassName: npu.llmfit.ai
    constraints:
      - requests: ["gpu", "npu"]
        matchAttribute: resource.kubernetes.io/pcieRoot
---
apiVersion: v1
kind: Pod
metadata:
  name: llmfit-aligned-consumer
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
      resourceClaimName: llmfit-aligned-claim
EOF
  kubectl wait --for=condition=Ready pod/llmfit-aligned-consumer --timeout=120s >/dev/null \
    || fail "aligned gpu+npu claim did not allocate/prepare (matchAttribute broken?)"
  count=$(kubectl get resourceclaim llmfit-aligned-claim -o jsonpath='{.status.allocation.devices.results}' | jq 'length')
  [ "$count" = 2 ] || fail "expected 2 allocated devices, got $count"
  env_key() { echo "LLMFIT_DEVICE_$(echo "$1" | tr 'a-z-' 'A-Z_')"; }
  gpu_env=$(kubectl exec llmfit-aligned-consumer -- sh -c "echo \$$(env_key "$GPU_DEV")")
  npu_env=$(kubectl exec llmfit-aligned-consumer -- sh -c "echo \$$(env_key "$NPU_DEV")")
  [ -n "$gpu_env" ] && [ -n "$npu_env" ] || fail "multi-device CDI merge lost a device (gpu='$gpu_env' npu='$npu_env')"
  kubectl exec llmfit-aligned-consumer -- test -e "$npu_env" || fail "npu device node $npu_env not injected"
  kubectl exec llmfit-aligned-consumer -- test -e "$gpu_env" || fail "gpu device node $gpu_env not injected"
  pass "gpu+npu aligned on one pcieRoot, both prepared into one pod (gpu=$gpu_env npu=$npu_env)"
else
  echo "  SKIP: needs both gpu0 and npu0 on the node"
fi

echo "== Scenario 12: uevent-triggered re-probe (hot-attach path)"
# The production driver runs unprivileged (no hostPID), so host-namespace
# operations use a throwaway privileged pod instead of the driver.
cleanup_host
kubectl -n kube-system run scenario-host --restart=Never --image=docker.io/library/busybox:1.36 \
  --overrides='{"spec":{"nodeName":"'"$node"'","hostPID":true,"containers":[{"name":"scenario-host","image":"docker.io/library/busybox:1.36","command":["sleep","600"],"securityContext":{"privileged":true}}]}}' >/dev/null
kubectl -n kube-system wait --for=condition=Ready pod/scenario-host --timeout=90s >/dev/null
host_exec() { kubectl -n kube-system exec scenario-host -- nsenter -t 1 -m -- sh -c "$1"; }
# Synthesize a kernel uevent by writing to a drm device's uevent file in
# the HOST mount namespace.
if host_exec 'ls /sys/class/drm/card*/uevent >/dev/null 2>&1'; then
  before=$(kubectl -n "$NS" logs ds/llmfit-dra | grep -c "uevent-triggered re-probe" || true)
  # kind nodes mount /sys read-only: remount rw just long enough to write
  # (echo into a uevent file makes the kernel emit that event for real).
  host_exec 'u=$(ls -d /sys/class/drm/card*/uevent | head -1); mount -o remount,rw /sys; rc=1; echo change > "$u" && rc=0; mount -o remount,ro /sys; exit $rc'
  triggered=""
  for i in $(seq 1 15); do
    after=$(kubectl -n "$NS" logs ds/llmfit-dra | grep -c "uevent-triggered re-probe" || true)
    if [ "$after" -gt "$before" ]; then triggered=1; break; fi
    sleep 2
  done
  [ -n "$triggered" ] || fail "no uevent-triggered re-probe within 30s (listener dead? hostNetwork missing?)"
  pass "synthesized drm uevent re-probed within seconds (count $before → $after)"
else
  echo "  SKIP: no /sys/class/drm devices on the host to synthesize a uevent from"
fi

echo "== Scenario 13: vendor coexistence — a vendor driver's presence demotes our GPUs"
if [ "$HAS_GPU" = 1 ]; then
  cleanup13
  node=$(our_slice | jq -r '.[0].spec.nodeName')
  kubectl apply -f - <<EOF >/dev/null
apiVersion: resource.k8s.io/v1
kind: ResourceSlice
metadata:
  name: fake-vendor-slice
spec:
  driver: gpu.nvidia.com
  nodeName: $node
  pool:
    name: $node
    resourceSliceCount: 1
  devices:
    - name: gpu-0
EOF
  demoted=""
  for i in $(seq 1 30); do
    vm=$(our_slice | jq --arg n "$GPU_DEV" -r '.[0].spec.devices[] | select(.name == $n) | .attributes.vendorManaged.bool // empty')
    [ "$vm" = "true" ] && { demoted=1; break; }
    sleep 2
  done
  [ -n "$demoted" ] || fail "GPU not marked vendorManaged within 60s of vendor slice appearing"
  # A default-class claim must now refuse the demoted GPU (stay unallocated).
  apply_consumer llmfit-coexist-consumer llmfit-coexist-claim gpu.llmfit.ai
  sleep 10
  alloc=$(kubectl get resourceclaim llmfit-coexist-claim -o jsonpath='{.status.allocation.devices.results[0].device}' 2>/dev/null || true)
  [ -z "$alloc" ] || fail "demoted GPU was allocated through the default class ('$alloc') — double-booking hazard"
  kubectl delete --ignore-not-found --grace-period=1 pod/llmfit-coexist-consumer >/dev/null 2>&1
  kubectl delete --ignore-not-found resourceclaim/llmfit-coexist-claim >/dev/null 2>&1
  # Vendor driver leaves: demotion clears and allocation works again.
  kubectl delete resourceslice fake-vendor-slice >/dev/null
  restored=""
  for i in $(seq 1 30); do
    vm=$(our_slice | jq --arg n "$GPU_DEV" -r '.[0].spec.devices[] | select(.name == $n) | .attributes.vendorManaged.bool // empty')
    [ -z "$vm" ] && { restored=1; break; }
    sleep 2
  done
  [ -n "$restored" ] || fail "vendorManaged attribute did not clear after vendor slice removal"
  pass "vendor presence demoted $GPU_DEV (default class refused it); removal restored it"
else
  echo "  SKIP: needs a fit-capable GPU"
fi

echo "== Scenario 14: admission policy scopes slice writes to the writer's node"
# The driver's own writes demonstrably work (scenarios 1/3). A request AS
# the driver SA but without a node-bound token (kubectl impersonation has
# no node-name extra) must be denied.
if kubectl get validatingadmissionpolicy llmfit-dra-slice-scope >/dev/null 2>&1; then
  deny_out=$(kubectl --as=system:serviceaccount:llmfit-dra:llmfit-dra apply -f - 2>&1 <<EOF || true
apiVersion: resource.k8s.io/v1
kind: ResourceSlice
metadata:
  name: forged-slice
spec:
  driver: llmfit.ai
  nodeName: some-other-node
  pool:
    name: some-other-node
    resourceSliceCount: 1
  devices:
    - name: gpu-fake
EOF
)
  echo "$deny_out" | grep -q "denied" || fail "forged cross-node slice was NOT denied: $deny_out"
  kubectl delete resourceslice forged-slice --ignore-not-found >/dev/null 2>&1
  pass "cross-node slice forgery denied by ValidatingAdmissionPolicy"
else
  echo "  SKIP: ValidatingAdmissionPolicy not installed (run 'make deploy')"
fi

echo "== Scenario 15: real compute on the claimed device (not just env vars)"
# The strongest assertion in the suite: a claimed pod opens the injected
# render node through a real userspace driver (Mesa RADV/ANV via Vulkan).
if [ "$HAS_GPU" = 1 ]; then
  cleanup15
  kubectl apply -f - <<'EOF' >/dev/null
apiVersion: resource.k8s.io/v1
kind: ResourceClaim
metadata:
  name: llmfit-compute-claim
spec:
  devices:
    requests:
      - name: dev
        exactly:
          deviceClassName: gpu.llmfit.ai
---
apiVersion: v1
kind: Pod
metadata:
  name: llmfit-compute
spec:
  restartPolicy: Never
  terminationGracePeriodSeconds: 1
  containers:
    - name: main
      image: docker.io/library/debian:trixie-slim
      command:
        - sh
        - -c
        - |
          set -e
          apt-get update -qq >/dev/null
          apt-get install -yqq vulkan-tools mesa-vulkan-drivers >/dev/null 2>&1
          vulkaninfo --summary
      resources:
        claims:
          - name: dev
  resourceClaims:
    - name: dev
      resourceClaimName: llmfit-compute-claim
EOF
  # Generous timeout: cold node = image pull + ~100MB Mesa install.
  kubectl wait --for=jsonpath='{.status.phase}'=Succeeded pod/llmfit-compute --timeout=600s >/dev/null \
    || { kubectl describe pod llmfit-compute | tail -8; kubectl logs llmfit-compute 2>/dev/null | tail -8; fail "compute pod did not succeed"; }
  gpu_line=$(kubectl logs llmfit-compute | grep -iE "deviceName" | grep -viE "llvmpipe|swiftshader" | head -1)
  [ -n "$gpu_line" ] || fail "vulkaninfo saw no physical GPU (software rasterizer only) — device injection is not usable"
  pass "Vulkan enumerated the claimed device:${gpu_line}"
else
  echo "  SKIP: needs a fit-capable GPU"
fi

echo
echo "All scenarios passed."
