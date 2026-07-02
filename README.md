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

**Build.** The binary is **not a GitHub release and not crates.io** — it is
built from local llmfit source and baked into the image:

1. `cargo build --release -p llmfit` in `LLMFIT_SRC` (default `~/Code/llmfit`).
2. `make image` copies `$(LLMFIT_SRC)/target/release/llmfit` to
   `third_party/llmfit` (gitignored) and the Dockerfile `COPY`s it in.

Local source is currently *required*: the integration depends on
`memory_bandwidth_gbps` in the `system --json` output, which landed on llmfit
`main` (AlexsJones/llmfit@3bfd334, with the Strix Halo bandwidth entries) and
is not yet in any tagged release. Two consequences to be aware of:

- **glibc coupling** — the binary is host-built, so the image's final base
  (`fedora-minimal:44`) must match the build host's glibc. Building on a
  different distro means changing the base image or building llmfit inside
  the Dockerfile.
- **Reproducibility** — the image embeds whatever is in your local llmfit
  working tree. Once a tagged llmfit release includes the bandwidth field,
  the plan is to pin a released artifact (or `ghcr.io/alexsjones/llmfit`)
  in a Dockerfile build stage instead.

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
(cd ~/Code/llmfit && cargo build --release -p llmfit)   # capability engine (see "How llmfit is consumed")
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
