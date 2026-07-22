# Examples

Copy-paste ResourceClaims for requesting devices from `llmfit-dra`. Each
file is self-contained (claim + a busybox pod that prints the injected
`LLMFIT_*` env), so you can apply it, watch it schedule, and see what the
kubelet plugin handed the container.

**Prerequisite:** the driver installed (see the repo README) and a cluster
on Kubernetes ≥ 1.34. First, look at what your nodes actually published —
this is your menu:

```sh
kubectl get resourceslices -o yaml   # one slice per node; .spec.devices[] lists cpu0, gpu0, npu0…
```

The four kind DeviceClasses (`cpu.llmfit.ai`, `gpu.llmfit.ai`,
`npu.llmfit.ai`, and the generic `llmfit.ai`) are the request vocabulary;
a claim selects **device entries** out of a node's slice, filtered by the
class and any CEL you add.

## The examples — start with the ModelClaim

| File | What it asks for | Runs on |
|------|------------------|---------|
| [`00-modelclaim.yaml`](00-modelclaim.yaml) | **a device that can run a model** (ModelClaim) | any — status explains fit |
| [`01-cpu-claim.yaml`](01-cpu-claim.yaml) | the CPU device, by class alone | every node |
| [`02-gpu-claim.yaml`](02-gpu-claim.yaml) | any healthy GPU, by class alone | nodes with a GPU |
| [`03-gpu-with-min-memory.yaml`](03-gpu-with-min-memory.yaml) | a GPU with ≥ 16Gi memory (class + CEL) | nodes with a big-enough GPU |
| [`04-npu-claim.yaml`](04-npu-claim.yaml) | the NPU device, by class alone | nodes with an NPU |
| [`05-gpu-plus-npu-aligned.yaml`](05-gpu-plus-npu-aligned.yaml) | a GPU **and** NPU on the same PCIe root | nodes with both |
| [`07-disaggregated-prefill-decode.yaml`](07-disaggregated-prefill-decode.yaml) | **a prefill pool and a decode pool**, each by its role's physics | multi-node fleets |
| [`08-llamacpp-proof-of-work.yaml`](08-llamacpp-proof-of-work.yaml) | **a real model served on the claimed silicon** (llama.cpp Vulkan) | GPU nodes — the full proof |
| [`10-gpu-plus-nic-aligned.yaml`](10-gpu-plus-nic-aligned.yaml) | a GPU **and** an RDMA NIC on the same PCIe root (GPUDirect-correct) | nodes with an RDMA HCA |
| [`11-nvidia-mig-modelclaim.yaml`](11-nvidia-mig-modelclaim.yaml) | **a MIG slice big enough for the model** (ModelClaim → NVIDIA translation backend) | NVIDIA DRA driver + static MIG |

**`00-modelclaim.yaml` is the intended way in** — name the model, let the
physics pick the device:

```sh
kubectl apply -f 00-modelclaim.yaml
kubectl get modelclaim qwen-coder            # RESOLVED / SATISFIABLE at a glance
kubectl describe modelclaim qwen-coder       # bounds, candidates, or the shortfall
kubectl logs llmfit-modelclaim-demo          # LLMFIT_DEVICE=… once Running
kubectl delete -f 00-modelclaim.yaml
```

The numbered device-kind claims (01–05) are the granular vocabulary
underneath — useful when you want a *specific* device rather than a model
fit. Most nodes have at least a CPU and a GPU, so 01 and 02 run anywhere:

```sh
kubectl apply -f 02-gpu-claim.yaml
kubectl wait --for=condition=Ready pod/llmfit-gpu-demo --timeout=120s
kubectl logs llmfit-gpu-demo        # --- llmfit env ---  LLMFIT_DEVICE=gpu0
kubectl delete -f 02-gpu-claim.yaml
```

If a device isn't present the pod stays **Pending** ("cannot allocate all
claims") — that's the honest signal that the node can't satisfy the request,
not an error.

## Ask for a model, not a device (ModelClaim)

Instead of naming a device, name the **model** — the capability no other
DRA driver has. Declaratively, with `00-modelclaim.yaml`:

```yaml
apiVersion: llmfit.ai/v1alpha1
kind: ModelClaim
metadata: { name: qwen-coder }
spec: { model: Qwen/Qwen2.5-Coder-3B-Instruct, minTps: 15 }
```

The controller resolves the physics and maintains a **same-named
ResourceClaimTemplate**; your pod just says
`resourceClaimTemplateName: qwen-coder`. `kubectl describe modelclaim`
shows the resolved bounds and whether any device in the cluster satisfies
them — including the exact shortfall when none does.

The imperative equivalent (no controller needed) is the generator shipped
in the driver image; it resolves the same bounds and writes the fit CEL
for you:

```sh
kubectl -n llmfit-dra exec ds/llmfit-dra -- \
  llmfit claim Qwen/Qwen2.5-7B --min-tps 20 | kubectl apply -f -
```

The emitted claim lands on whichever device on the node satisfies the
inequality (memory ≥ weights, bandwidth ≥ the floor for your target tok/s,
healthy) — or nothing, if the node genuinely can't run it at that speed.
Add `--template` to emit a `ResourceClaimTemplate` for a Deployment instead
of a one-shot claim.

## Real-world shape: disaggregated prefill/decode

[`07-disaggregated-prefill-decode.yaml`](07-disaggregated-prefill-decode.yaml)
shows how a production serving topology falls out of ModelClaims naturally.
Prefill and decode want opposite hardware — prefill is compute-bound and
memory-hungry (long-context KV), decode is pure memory bandwidth (which is
exactly what `minTps` encodes) — so each role gets its own ModelClaim for
the **same model**, and the scheduler sorts a heterogeneous fleet into
pools with no node labels or affinity rules:

- `qwen-decode`: `minTps: 25`, `deviceClassName: gpu.llmfit.ai` — the
  bandwidth floor picks the fast silicon, never the CPU fallback.
- `qwen-prefill`: `minTps: 5` + an `extraSelectors` memory floor — fits
  anywhere with enough room for deep KV.

The KV-transfer path between pools (NIXL/UCX, vLLM's
`--kv-transfer-config`, Dynamo) belongs to the serving layer inside the
containers — llmfit-dra places the roles and stays out of the data path.

**Co-located variant** (single node with two accelerators — e.g. GPU +
NPU): one claim, two requests, aligned on the PCIe root, and each
container binds its own request by name:

```yaml
spec:
  resourceClaims:
    - name: accel
      resourceClaimName: pd-aligned-claim   # requests: prefill, decode (05-style)
  containers:
    - name: prefill
      resources:
        claims: [{ name: accel, request: prefill }]
    - name: decode
      resources:
        claims: [{ name: accel, request: decode }]
```

## Using a claim from your own pod

Every example wires the pod to the claim the same way — the request `name`
inside the claim is what `resources.claims` references:

```yaml
spec:
  resourceClaims:
    - name: accel
      resourceClaimName: gpu-claim      # any claim from above
  containers:
    - name: main
      image: your/image
      resources:
        claims: [{ name: accel }]
```

The kubelet plugin injects `LLMFIT_DEVICE=<name>` (env-only for CPU; env +
`/dev` nodes for GPU/NPU) so your container knows which silicon it got.
