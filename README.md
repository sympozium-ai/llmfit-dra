# llmfit-dra

A Kubernetes [DRA](https://kubernetes.io/docs/concepts/scheduling-eviction/dynamic-resource-allocation/)
driver that publishes each node's accelerator inventory as **ResourceSlices**
under the `llmfit.ai` driver name — the Kubernetes-native successor to
node-annotation hardware advertisement.

The design thesis (from llmfit): **probe ⋈ index**. Identity is discoverable
from the device tree; capability is not. The probe walks `/sys/class/drm`,
`/sys/class/accel` and procfs; the curated index maps PCI IDs to what the OS
can't tell you — memory bandwidth, marketing name, unified-memory semantics.
Everything else is a consumer of that join.

**Phase 1 (this repo): publish-only.** No kubelet plugin and no shipped
DeviceClass, so devices are visible to the scheduler, Kueue, and controllers —
and CEL allocation demonstrably works (scenario 4) — but pods cannot run
against claims yet. That is deliberate: inventory first, allocation later.

**Stage two: the real llmfit is the capability source.** The image ships the
llmfit binary (Rust); the DaemonSet runs privileged with host `/sys`, `/dev`
and the host PID namespace, and the publisher shells out to
`llmfit --json system` each probe cycle. llmfit owns detection nuance the
generic probe can't (APU unified-memory pools, nvidia-smi/rocm-smi/lspci
fallbacks, its bandwidth database); the Go probe still supplies PCI identity
(address, root, driver), and the embedded index remains the fallback when
llmfit is unavailable or doesn't model a device class (e.g. XDNA NPUs).
Provenance is explicit in the `source` attribute: `llmfit` | `index` | `probe`.

## What gets published

One ResourceSlice per node (pool = node name). Devices are named `gpu0…N`,
`npu0…N`, `cpu0`; identity lives in attributes, never names.

| Attribute (`llmfit.ai/…`) | Type | Example |
|---|---|---|
| `kind` | string | `gpu` \| `npu` \| `cpu` |
| `vendor` | string | `intel`, `nvidia`, `amd`, `cpu` |
| `model` | string | `Intel Arc Graphics 140V` (from index) |
| `driver` | string | `xe`, `intel_vpu` |
| `pciAddress` / `pcieRoot` | string | `0000:00:02.0` / `pci0000:00` — correlation keys to vendor DRA drivers |
| `memoryBandwidthGBs` | int | `136` — the number tok/s physics hangs off; **index-sourced, not OS-discoverable** |
| `unifiedMemory` | bool | `true` on iGPUs/Apple-class devices |
| `indexed` | bool | whether the capability index recognized the PCI ID |
| `source` | string | capability provenance: `llmfit` \| `index` \| `probe` |
| `backend` | string | llmfit inference backend: `Vulkan`, `CUDA`, `ROCm`, `SYCL`, `Metal` |
| `healthy` | bool | placeholder `true` (XID/NVML event wiring is roadmap) |
| capacity `memory` | quantity | VRAM, or system RAM when unified |

Fit for a *specific* LLM is deliberately **not** published (models × devices
cardinality, instant staleness). The physics inputs are; the fit inequality
belongs in a CEL selector at claim time:

```yaml
selectors:
  - cel:
      expression: >-
        device.attributes["llmfit.ai"].memoryBandwidthGBs >= 100 &&
        device.capacity["llmfit.ai"].memory.compareTo(quantity("8Gi")) >= 0
```

## How llmfit is consumed

Two couplings, one at runtime and one at build time:

**Runtime.** The Go driver does not link llmfit — it shells out to
`llmfit --json system` (path from `LLMFIT_BIN`, baked into the image as
`/usr/local/bin/llmfit`) on every probe cycle and parses the JSON
(`internal/llmfit`). If the exec fails or llmfit doesn't model a device class,
the publisher degrades to the embedded index — the `source` attribute on every
device tells you which path was taken.

**Build.** llmfit is a **git submodule** at `third_party/llmfit`, pinned to an
exact commit (currently AlexsJones/llmfit@`3bfd334` — the first commit with
`memory_bandwidth_gbps` in the `system --json` output plus the Strix Halo
bandwidth entries; no tagged llmfit release has these yet). The Dockerfile is
hermetic: a `rust:1-slim-bookworm` stage compiles the submodule with
`cargo build --release -p llmfit`, and the runtime stage
(`debian:bookworm-slim` + pciutils) copies in both binaries. No host toolchain
leaks into the image, so builds are reproducible from a clean
`git clone --recurse-submodules`.

To bump the pin: `cd third_party/llmfit && git fetch && git checkout <ref>`,
commit the submodule update, and CI rebuilds against it. Once a tagged llmfit
release includes the bandwidth field, the pin can track release tags.

**CI / images.** `.github/workflows/build.yml` runs vet + unit tests, then
builds and pushes `ghcr.io/sympozium-ai/llmfit-dra` on every push to `main`
(tags: `latest`, `main`, `sha-<short>`) and on `v*` tags (semver). PRs build
without pushing. linux/amd64 only for now — the llmfit Rust stage under QEMU
makes arm64 impractical; revisit with native arm64 runners.

## Layout

```
cmd/llmfit-dra/       main: flags, kube client, probe loop
internal/probe/       device tree walk (sysfs/procfs, root-parameterized for tests)
internal/index/       embedded capability index (data.json)
internal/publisher/   probe ⋈ index → resource.k8s.io/v1 Devices; resourceslice helper wiring
deploy/               namespace, RBAC, DaemonSet
hack/scenarios.sh     live-cluster scenarios (publish / shape / reconcile / CEL-consume)
```

## Quickstart (kind)

Requires Kubernetes ≥ 1.34 (DRA GA). Tested on kind + v1.35.

```sh
git clone --recurse-submodules git@github.com:sympozium-ai/llmfit-dra.git
make test                        # unit tests
make kind-load KIND_CLUSTER=tailnet   # kind on this machine…
make sideload                         # …or remote kind reachable only via kubectl
make deploy
kubectl get resourceslices -o yaml   # inspect the published inventory
make scenarios                   # end-to-end assertions
```

`make sideload` streams `docker save` through a temporary privileged pod into
the node's containerd (`ctr -n k8s.io images import`) — for clusters whose
docker/kind daemon lives on another host (e.g. over tailscale).

## Scenarios

1. **Publish** — DaemonSet up, ResourceSlice exists with `spec.driver: llmfit.ai` and correct `nodeName`.
2. **Shape** — `gpu0`/`cpu0` present; vendor/unifiedMemory/indexed/bandwidth/capacity assertions.
3. **Reconcile** — deleting the slice; the helper controller recreates it (event-driven desired-state sync).
4. **Consume** — a DeviceClass + ResourceClaim with a CEL fit expression gets **allocated by the stock kube-scheduler** against our attributes. The pod then parks in ContainerCreating — expected, since Phase 1 ships no kubelet plugin.

## Roadmap

- **Phase 2**: kubelet DRA plugin (`NodePrepareResources` → CDI), shipped DeviceClass, `llmfit claim <model>` CLI generating fit CEL from the llmfit model database.
- **Phase 3**: cross-driver `matchAttribute` alignment (`pcieRoot`) with vendor drivers; health events (XID/DCGM → attribute flip / device taints); udev/netlink hot-attach triggering instead of periodic re-probe.
- **Index as artifact**: extract `internal/index/data.json` into a versioned dataset other drivers can vendor.

Design doc: *POC — llmfit as a DRA ResourceSlice Publisher* (Obsidian, sympozium vault).
