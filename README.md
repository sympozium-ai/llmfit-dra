# llmfit-dra

**Run LLMs on the right accelerator using nothing but the Kubernetes
scheduler.**

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
llmfit claim <model> ──► ResourceClaim (fit as CEL) ──► kube-scheduler picks
the node/device whose physics satisfy it ──► kubelet plugin injects
/dev nodes + env ──► your pod runs on silicon that can actually hold the model
```

## Getting started

Requires Kubernetes ≥ 1.34 (DRA GA). The image is on GHCR (currently
private — create a pull secret with a `read:packages` token; `make
pull-secret` does this).

```sh
helm install llmfit-dra charts/llmfit-dra -n llmfit-dra --create-namespace
kubectl get resourceslices        # your accelerator inventory, as API objects
```

Ask for a model instead of a device. The generator resolves weights size and
bandwidth floor from llmfit's model database and emits plain YAML (the
binary ships in the driver image):

```sh
kubectl -n llmfit-dra exec ds/llmfit-dra -- \
  llmfit claim Qwen/Qwen2.5-7B --min-tps 20 | kubectl apply -f -
```

```yaml
# fit: tok/s ≈ bandwidth × 55% / 4.4 GB ⇒ bandwidth ≥ 161 GB/s for ≥ 20 tok/s
kind: ResourceClaim
spec:
  devices:
    requests:
      - name: model
        exactly:
          deviceClassName: llmfit.ai
          selectors:
            - cel: { expression: "... memory ≥ 5Gi && bandwidth ≥ 161 && healthy ..." }
```

Reference it from any pod (`--template` emits a ResourceClaimTemplate for
Deployments):

```yaml
spec:
  resourceClaims:
    - name: model
      resourceClaimName: qwen-qwen2-5-7b-fit
  containers:
    - name: main
      resources:
        claims: [{ name: model }]
```

The pod lands on a fitting device with its `/dev` nodes injected and
`LLMFIT_DEVICE*` env set. No llmfit component is in the serving path — every
artifact above is a plain Kubernetes object.

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

The driver serves Prometheus metrics and health on `nodeIP:9099`
(`/metrics`, `/healthz`, `/readyz`) — `capability_source`, degraded-cycle,
probe-latency, and prepare/unprepare counters.

Everything is a Helm value: `image.*`, `metricsPort`, `probeInterval`, `kubeletPlugin`
(false = publish-only inventory), `vendorDrivers` (coexistence list),
`publishTaints` (needs the alpha `DRADeviceTaints` gate),
`deviceClasses.enabled`, `admissionPolicy.enabled` (scopes the driver's
slice writes to its own node), tolerations/priority. See
`charts/llmfit-dra/values.yaml`.

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

## Status & roadmap

Validated end-to-end on AMD (Strix Halo APU + XDNA NPU) and in CPU-only CI,
including real Vulkan compute in a claimed pod; hardened by an adversarial
readiness audit (`docs/readiness-audit-2026-07-02.json`). On NVIDIA/Neuron
nodes the driver deliberately defers allocation to the vendor's DRA driver
(coexistence above). Next: evidence on a second vendor's silicon, live
cross-driver `matchAttribute`, and extracting the capability index as a
standalone dataset.

Design doc: *POC — llmfit as a DRA ResourceSlice Publisher* (Obsidian,
sympozium vault).
