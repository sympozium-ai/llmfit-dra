# llmfit-dra

**Run LLMs on the right accelerator using nothing but the Kubernetes
scheduler.**

```yaml
# "Run Qwen3.6 at ≥20 tok/s" — as a Kubernetes object.
apiVersion: llmfit.ai/v1alpha1
kind: ModelClaim
metadata: { name: qwen36 }
spec:
  model: Qwen/Qwen3.6-30B-A3B
  minTps: 20
```

```console
$ kubectl get modelclaim qwen36
NAME     MODEL                  MINTPS   RESOLVED   SATISFIABLE   DEVICES
qwen36   Qwen/Qwen3.6-30B-A3B   20       True       True          2
```

That's the value proposition in one object: you name the **model**, the
driver resolves the physics (weights size, bandwidth floor for your
tok/s target) from llmfit's database, and the stock kube-scheduler
places your pod on silicon that can actually run it. **No other DRA
driver can take this request** — they allocate by spec sheet (memory
quantities, product names); llmfit-dra allocates by what the hardware
can *do*. And when nothing fits, `kubectl describe modelclaim` says
exactly why (`closest device gpu-…: bandwidth 256 < 640 GB/s`) — before
any pod exists.

Kubernetes can allocate GPUs, but it has no idea whether a model *fits* one:
whether the weights fit in device memory, or whether the memory bandwidth
can hit your tokens-per-second target. That knowledge lives in
[llmfit](https://github.com/AlexsJones/llmfit)'s hardware and model
databases. llmfit-dra puts it where scheduling decisions happen: a
[DRA](https://kubernetes.io/docs/concepts/scheduling-eviction/dynamic-resource-allocation/)
driver (`llmfit.ai`) that publishes every node's accelerators — GPUs, NPUs,
and a CPU fallback — as typed **ResourceSlices**, and a kubelet plugin that
wires the allocated device into your pod via CDI. The stock kube-scheduler
does the placement; no custom scheduler, no annotations, no webhooks.

```
ModelClaim "run Qwen3.6 at ≥20 tok/s"
   ──► controller resolves weights + bandwidth floor from llmfit's model DB
   ──► same-named ResourceClaimTemplate (the physics, inlined as CEL)
   ──► kube-scheduler picks the node/device that satisfies it
   ──► kubelet plugin injects /dev nodes + env
   ──► your pod runs on silicon that can actually hold the model
```

## Positioning: the layer this is

llmfit-dra is the **capability layer**: it decides *where compute happens*.
It sits between two neighbours it deliberately does not replace:

| Layer | Owns | Decides |
|-------|------|---------|
| [Sympozium](https://github.com/sympozium-ai/sympozium) — coordination | Agents: identity, execution, policy, ensembles | What agents **do** |
| **llmfit-dra** — capability | Accelerator inventory, fit physics, claims, placement | Where compute **happens** |
| Serving engines (vLLM, SGLang, llama.cpp) — runtime | Batching, KV cache, disaggregation | How tokens **move** |

A ModelClaim is to llmfit-dra what a PersistentVolumeClaim is to a CSI
driver: any orchestrator (Sympozium included) *claims* a model the way an
application claims a volume, and the stock scheduler satisfies the physics.
llmfit-dra is never in the serving path — it places prefill-grade and
decode-grade *pools* (see `examples/07` and `examples/09`), and the serving
engine moves the tokens between them.

## Getting started

Requires Kubernetes ≥ 1.34 (DRA GA). The image **and** the Helm chart are
published to GHCR (currently private — you need a `read:packages` token).

Install the published chart straight from the registry (no checkout needed):

```sh
# 1. Log Helm in to the private registry (read:packages token)
echo "$GHCR_TOKEN" | helm registry login ghcr.io -u "$USER" --password-stdin

# 2. Install the chart — see Releases for the latest version
helm install llmfit-dra oci://ghcr.io/sympozium-ai/charts/llmfit-dra \
  --version 0.2.4 -n llmfit-dra --create-namespace

kubectl get resourceslices        # your accelerator inventory, as API objects
```

The driver image is private too, so the pods need an image pull secret —
`make pull-secret` creates one from a `read:packages` token (via
`GITHUB_TOKEN` or `gh auth token`).

Working from a checkout instead? Install the chart from the local path:

```sh
helm install llmfit-dra charts/llmfit-dra -n llmfit-dra --create-namespace
```

Ask for a **model** instead of a device — the request no other DRA driver
can take. A `ModelClaim` resolves the model's weights size and bandwidth
floor from llmfit's database and maintains a same-named
`ResourceClaimTemplate` with the physics inlined as CEL:

```yaml
apiVersion: llmfit.ai/v1alpha1
kind: ModelClaim
metadata: { name: qwen36 }
spec:
  model: Qwen/Qwen3.6-30B-A3B
  minTps: 20
```

```sh
kubectl get modelclaim qwen36
# NAME     MODEL                  MINTPS  RESOLVED  SATISFIABLE  DEVICES
# qwen36   Qwen/Qwen3.6-30B-A3B   20      True      True         2
```

`kubectl describe` shows the resolved bounds (`memory>=18Gi,
bandwidth>=160GB/s @ Q4_K_M`) and — when nothing fits — the exact
shortfall (`closest device gpu-…: bandwidth 256 < 640 GB/s`), *before any
pod exists*. Reference it from any pod by the same name:

```yaml
spec:
  resourceClaims:
    - name: model
      resourceClaimTemplateName: qwen36   # == the ModelClaim's name
  containers:
    - name: main
      resources:
        claims: [{ name: model }]
```

Prefer no controller? The imperative twin emits the same physics as plain
YAML (the binary ships in the driver image):

```sh
kubectl -n llmfit-dra exec ds/llmfit-dra -- \
  llmfit claim Qwen/Qwen2.5-7B --min-tps 20 | kubectl apply -f -
```

The pod lands on a fitting device with its `/dev` nodes injected and
`LLMFIT_DEVICE*` env set. No llmfit component is in the serving path — every
artifact above is a plain Kubernetes object.

New to this? [`examples/`](examples/) has copy-paste, self-verifying claims
for each device kind (start with CPU and GPU — most nodes have both), plus
the request-by-attribute and aligned multi-device patterns.

## What gets published

One ResourceSlice per node. Devices are named by PCI address
(`gpu-0000-c3-00-0`) so identity survives reboots; `cpu0` is the fallback
that makes everything work on accelerator-less nodes. Key attributes
(`llmfit.ai/…`):

| Attribute | Meaning |
|---|---|
| `kind`, `vendor`, `model`, `driver` | identity (`gpu`/`npu`/`cpu`, kernel driver, marketing name) |
| `memoryBandwidthGBs` | the number tok/s physics hangs off — curated, not OS-discoverable |
| capacity `memory` | VRAM (or the shared pool on unified-memory APUs); omitted when unknown |
| `healthy` / `healthReason` | driver bound, no uncorrectable RAS errors |
| `vendorManaged` | set when a vendor DRA driver (NVIDIA/Intel/Neuron) owns this node's GPUs — default classes then exclude them, so silicon is never double-booked |
| `resource.kubernetes.io/pcieRoot` | standardized key for cross-driver `matchAttribute` alignment |

Per-model fit scores are deliberately **not** published (models × devices
cardinality, instantly stale) — the fit inequality lives in the claim's CEL,
generated at claim time. Capability comes from the llmfit binary each probe
cycle, falling back to an embedded PCI-ID index (`source` attribute shows
which); identity comes from sysfs. Inventory updates are event-driven
(kernel uevents) with a periodic reconcile floor.

## Configuration

Both components serve Prometheus metrics and health (`/metrics`, `/healthz`,
`/readyz`): the node agent on `nodeIP:9099`, the ModelClaim controller on its
pod port. Headliners: `llmfit_dra_devices{kind,vendor,driver,healthy}` (the
published inventory itself — one query answers "what does the fleet look
like"), `capability_source`, `slice_publish_errors_total`,
`prepare_total{result,reason}`, and `resolve_duration_seconds`. Scrape
discovery is `prometheus.io/*` annotations by default; set
`metrics.podMonitor.enabled=true` for prometheus-operator. See
`docs/ARCHITECTURE.md` § Observability for the full inventory and
operational caveats (uninstall cleanup, non-root device permissions,
NVIDIA `nvidia_drm` requirement).

Everything is a Helm value: `image.*`, `metricsPort`, `probeInterval`, `kubeletPlugin`
(false = publish-only inventory), `vendorDrivers` (coexistence list),
`publishTaints` (needs the alpha `DRADeviceTaints` gate),
`deviceClasses.enabled`, `admissionPolicy.enabled` (scopes the driver's
slice writes to its own node), tolerations/priority. See
`charts/llmfit-dra/values.yaml`.

### Running on a subset of nodes (NFD)

By default the DaemonSet runs everywhere — the `cpu0` fallback is what makes
accelerator-less nodes useful. If you want an accelerator-only deployment and
the cluster already runs
[node-feature-discovery](https://github.com/kubernetes-sigs/node-feature-discovery)
(the NVIDIA GPU Operator ships it), pin the agent with a hardware-presence
label:

```yaml
# values.yaml — pick the label your cluster actually has
nodeSelector:
  feature.node.kubernetes.io/pci-0300_1002.present: "true"  # NFD: AMD display-class device
# nodeSelector:
#   nvidia.com/gpu.present: "true"                          # GPU Operator equivalent
```

That's the extent of the integration — llmfit-dra never reads node labels
itself. Capability (VRAM, bandwidth, health) is published as per-device
ResourceSlice attributes because that's what DRA CEL can evaluate; node
labels structurally can't participate in the fit. Vendor coexistence is
likewise detected live — does a vendor driver publish slices for this
node? — rather than inferred from hardware-presence labels
(`vendorDrivers`).

## Development

```sh
git clone --recurse-submodules git@github.com:sympozium-ai/llmfit-dra.git
make test                   # unit tests
make deploy-local TAG=dev   # build working tree → sideload into kind → helm install
make scenarios              # 15-scenario live e2e suite (see hack/scenarios.sh)
make help                   # everything else
```

- `internal/probe` walks sysfs (identity, /dev nodes, health);
  `internal/publisher` joins probe ⋈ index ⋈ llmfit into ResourceSlices;
  `internal/nodeplugin` serves the kubelet DRA plugin (CDI); llmfit is a
  git submodule pinned to a release tag and built into the image.
  **[docs/ARCHITECTURE.md](docs/ARCHITECTURE.md)** has the full picture —
  the two planes, the capability-source chain, device identity, coexistence,
  and the security model, with diagrams.
- CI runs unit tests (`-race`), lints the chart, and helm-installs onto a
  3-node kind cluster for the full scenario suite (GPU assertions self-skip
  there; `make scenarios-cpu` reproduces that mode on any cluster).
- Releases are cut by release-please from conventional commits: merging the
  release PR tags `vX.Y.Z`, builds the matching image, and publishes the
  chart to `oci://ghcr.io/sympozium-ai/charts/llmfit-dra`.

Design doc: *POC — llmfit as a DRA ResourceSlice Publisher* (Obsidian,
sympozium vault).
