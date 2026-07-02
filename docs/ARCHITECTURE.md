# Architecture

llmfit-dra is a Kubernetes [Dynamic Resource Allocation](https://kubernetes.io/docs/concepts/scheduling-eviction/dynamic-resource-allocation/)
(DRA) driver under the name `llmfit.ai`. It exists to answer one question the
stock scheduler cannot: *does this LLM fit this accelerator?* — and to answer
it using only native Kubernetes objects, so the ordinary kube-scheduler makes
the placement.

This document explains how the pieces fit together. For *why* the project
exists and how to install it, see the [README](../README.md); for the
original design rationale, the *POC — llmfit as a DRA ResourceSlice
Publisher* note in the sympozium Obsidian vault.

## The thesis: probe ⋈ index ⋈ llmfit

Identity is discoverable from the OS; capability is not. A GPU's PCI address,
kernel driver, and `/dev` nodes are all readable from sysfs — but the memory
bandwidth that decides tokens-per-second, or whether a device shares a
unified memory pool, is not. That knowledge lives in curated databases.

The driver therefore **joins three sources**, each authoritative for
different columns:

```
  probe            index                    llmfit (serve API / exec)
  ─────            ─────                     ────────────────────────
  PCI address      marketing name           real assessment: APU pools,
  kernel driver    memory bandwidth         vendor-tool fallbacks
  /dev nodes       unified-memory flag      (nvidia-smi/rocm-smi/lspci),
  VRAM (sysfs)     (fallback for the        its own bandwidth database
  RAS / health      above when llmfit
                    is unavailable)

        └──────────────┴─── identity from probe, always ───┘
                     capability: llmfit ▸ index ▸ probe
```

`internal/publisher` performs the join. The `source` attribute on every
published device records which capability path won (`llmfit` | `index` |
`probe`), so the provenance is never ambiguous.

## Two planes

The driver has a **control plane** (publish inventory) and a **data plane**
(prepare allocated devices). They are independent: the publisher makes
devices *schedulable*; the kubelet plugin makes them *usable*.

```
                    ┌────────────────────── llmfit-dra pod (per node, DaemonSet) ──────────────────────┐
                    │                                                                                   │
  /sys, /proc ─────▶│  probe ─┐                                        ┌── llmfit sidecar ──┐           │
  /dev             │         ├─▶ publisher ──▶ ResourceSlice ◀────┐   │  llmfit serve       │           │
                    │  index ─┘   (probe ⋈ index ⋈ llmfit)         │   │  --unix-socket      │           │
                    │              │                    ▲          │   └─────────┬──────────┘           │
                    │              │ capability         │          │             │ AF_UNIX              │
                    │              └── Detector ◀────────┼──────────┼─────────────┘  /run/llmfit/*.sock  │
                    │                                    │          │                                    │
  uevents ─────────▶│  hotplug ── triggers re-probe ─────┘          │                                    │
                    │                                               │                                    │
  kubelet ─────────▶│  nodeplugin (NodePrepareResources) ──▶ CDI spec in /var/run/cdi                    │
                    │       └─ joins allocation result ⋈ probe inventory by device name                  │
                    └───────────────────────────────────────────────────────────────────────────────────┘
                                    │                                        │
              CONTROL PLANE ────────┘                                        └──────── DATA PLANE
              ResourceSlice → kube-scheduler                          CDI edits → containerd → pod
```

### Package map

| Package | Responsibility |
|---|---|
| `cmd/llmfit-dra` | flags, kube client, the probe/publish loop, plugin + hotplug wiring |
| `internal/probe` | sysfs/procfs walk: identity, `/dev` nodes, driver binding, RAS health (root-parameterized for tests) |
| `internal/index` | embedded PCI-ID → capability table (`data.json`) |
| `internal/llmfit` | capability source: `Detector` (API → exec → last-known-good), `unix://` client, exec shim |
| `internal/publisher` | the probe ⋈ index ⋈ llmfit join → `resource.k8s.io/v1` devices; vendor coexistence |
| `internal/nodeplugin` | kubelet DRA plugin: prepare/unprepare, CDI spec generation, inventory join |
| `internal/hotplug` | netlink uevent listener → event-driven re-probe |
| `charts/llmfit-dra` | Helm chart (the supported install path) |
| `deploy/` | equivalent raw manifests for `kubectl apply` |
| `third_party/llmfit` | llmfit (Rust), git submodule pinned to a release tag, built into the image |

## Control plane: publishing inventory

`cmd/llmfit-dra` runs one loop per node. Each cycle:

1. **probe** walks `/sys/class/drm` (GPUs), `/sys/class/accel` (NPUs), and
   procfs (the CPU fallback device), producing normalized `probe.Device`s.
2. **detect** asks the `Detector` for capability (see next section).
3. **publish** joins probe ⋈ index ⋈ llmfit into `resourceapi.Device`s and
   hands the desired state to the upstream `resourceslice` helper controller,
   which diffs against the API server and writes only on change.

```
                  ┌──────── one iteration ────────┐
   ┌── ticker ───▶│ probe.Walk → Detector.Detect  │
   │ (reconcile   │      → publisher.BuildDevices  │──▶ controller.Update(desiredState)
   │  floor)      │      → resourceslice helper    │        │
   │              └───────────────────────────────┘        ▼
   └── uevent ────────────────────────────────────▶   ResourceSlice (one per node)
       (hotplug)                                        owned by the Node object
```

The loop is **event-driven with a reconcile floor**: kernel uevents on the
`drm`/`accel` subsystems trigger an immediate re-probe (hot-attach, driver
bind/unbind, error events), while a ticker guarantees eventual convergence
even if events are missed. An unchanged desired-state push issues no API
writes, so pushing every cycle is cheap and self-heals externally deleted
slices.

### The capability source: a resilient chain

`internal/llmfit.Detector` is the single entry point the publisher calls. It
tries transports in preference order and caches the last good result:

```
  Detector.Detect(ctx)
     │
     ├─▶ 1. serve API  (unix:///run/llmfit/llmfit.sock, GET /api/v1/system)   ── preferred
     │       long-lived sidecar, versioned contract, no fork per cycle
     │
     ├─▶ 2. exec       (llmfit --json system)                                  ── fallback
     │       covers sidecar warmup and any API outage
     │
     ├─▶ 3. last-known-good  (within 10 min)                                   ── anti-flap
     │       a transient failure must NOT flip source llmfit→index and churn
     │       every slice fleet-wide
     │
     └─▶ (else) nil → publisher degrades to the embedded index
```

**Why a Unix socket, not TCP.** The pod runs `hostNetwork: true` (the uevent
netlink socket is network-namespace scoped and needs the host netns). A TCP
`llmfit serve` — even on `127.0.0.1` — would therefore bind the *node's*
loopback: a host-wide port, prone to collision and locally reachable by any
process on the box. A Unix socket lives on a pod-private `emptyDir` shared
only between the driver and sidecar containers; it does not exist on the host
network at all. Access control is mount topology plus file mode (`0660`),
not a firewall.

The sidecar is the **same image** running `llmfit serve --unix-socket …`,
with an `exec` liveness probe (`curl --unix-socket … /health`, because the
kubelet's `httpGet` cannot target a UDS).

## Data plane: preparing a device

Nothing runs here until the scheduler allocates a claim. Then the kubelet
calls the plugin, which turns the allocation into CDI container edits.

```
  llmfit claim <model>                         (generator: fit CEL from model DB)
        │  YAML
        ▼
  ResourceClaim  ──▶  kube-scheduler evaluates the CEL against published
  (deviceClassName    attributes across all nodes' ResourceSlices, picks a
   + fit selector)    device, writes allocation.result{driver, pool, device}
        │
        ▼
  kubelet ──▶ nodeplugin.PrepareResourceClaims
        │        join allocation.device (a stable name) ⋈ current probe inventory
        │        editsFor(device) → CDI spec written atomically to /var/run/cdi
        ▼
  containerd reads the CDI spec ──▶ injects /dev nodes + LLMFIT_* env ──▶ pod runs
```

`editsFor` produces the container edits for a device:

- **device nodes**: the DRM render node and card node for GPUs, plus
  `/dev/kfd` for amdgpu (ROCm needs it; the render node alone only covers
  Vulkan); `/dev/accel/accelN` for NPUs. `cpu0` injects no device nodes.
- **env**: `LLMFIT_DEVICE`, `LLMFIT_DEVICE_KIND`, `LLMFIT_RENDER_NODE`, and a
  per-device `LLMFIT_DEVICE_<NAME>=<node>` key that survives the CDI merge
  when a single claim requests multiple devices.

The CDI spec file **is the prepare state**. Unprepare is "delete the file";
a restart needs no checkpoint; a crash between write and kubelet-checkpoint
is cleaned up by a startup GC that removes specs whose ResourceClaim no
longer exists.

## Device identity

DRA allocations and the plugin's prepare **join on device names**, so a name
must identify the same silicon across reboots, hot-remove, and driver
reloads. An enumeration counter (`gpu0`, `gpu1`, …) does not — removing an
earlier card renumbers the survivors and silently rebinds live allocations
to different hardware.

Devices are therefore named by **PCI address**: `gpu-0000-c3-00-0`,
`npu-0000-c4-00-1`. The CPU fallback stays `cpu0` (a stable singleton). This
was the keystone fix from the readiness audit
(`docs/readiness-audit-2026-07-02.json`).

## DeviceClasses and vendor coexistence

Shipped classes: a base `llmfit.ai` and per-kind `gpu.llmfit.ai` /
`npu.llmfit.ai` / `cpu.llmfit.ai`. A class selector ANDs with the claim's
selector — the class guarantees "an llmfit device of this kind", the claim
adds the model-specific fit inequality. The class names also give Kueue and
ResourceQuota a countable vocabulary. Per-model fit is **never** a published
attribute (models × devices cardinality, instant staleness); it is CEL in the
claim, generated by `llmfit claim`.

**Coexistence.** On a node where a vendor DRA driver (`gpu.nvidia.com`,
`gpu.intel.com`, `neuron.amazonaws.com`) already publishes GPUs, allocating
the same silicon through llmfit too would double-book it. Each cycle the
publisher checks for a vendor driver's ResourceSlices on this node (one
field-selected list); if present, our GPUs gain a `vendorManaged` attribute
and the shipped base/gpu classes exclude them. The attributes remain
published, so a custom class *without* the exclusion can still use them as a
fitness companion via `matchAttribute`.

```
  gpu.nvidia.com slice present on node ─▶ publisher sets vendorManaged=true on our GPUs
                                          ─▶ default classes: !('vendorManaged' in attrs)
                                          ─▶ our GPU is fitness-only; NVIDIA owns allocation
```

## Health and hotplug

`healthy` is computed each probe cycle from facts: a device with no bound
kernel driver cannot be prepared (`driverUnbound`); amdgpu RAS reporting
uncorrectable ECC errors means the memory lies (`uncorrectableEcc`). The
reason is published in `healthReason`. `--publish-taints` additionally emits
a `NoSchedule` device taint (gated because `DRADeviceTaints` is alpha).

`internal/hotplug` binds `NETLINK_KOBJECT_UEVENT` (kernel group only —
udevd's rebroadcast is filtered), watches the `drm`/`accel` subsystems, and
debounces bursts on the trailing edge so a re-probe sees settled state. It
survives `ENOBUFS` storms by forcing a re-probe and continuing. Vendor event
streams (XID/DCGM) are deliberately out of scope — that is the vendor
driver's job on nodes where one runs.

## Observability and day-2 operations

The driver is built to degrade quietly, so it must report loudly. It serves
`/metrics`, `/healthz`, and `/readyz` on a hostNetwork port (default `:9099`,
scrape at `nodeIP:9099`):

- **Metrics** (`internal/observe`): `capability_source{source=…}` (1 on the
  active transport, so a fleet-wide fallback is one query),
  `degraded_cycles_total`, `probe_duration_seconds`, `slice_updates_total`,
  and `prepare_total{result}` / `unprepare_total{result}`.
- **Liveness** (`/healthz`) fails if the reconcile loop hasn't completed a
  cycle within three probe intervals — a hung loop becomes a failing probe,
  not a silent 1/1-Running node. **Readiness** (`/readyz`) goes true only
  after the publisher and plugin start.

**Rollout.** The DaemonSet uses `maxUnavailable: 1` (not `maxSurge` — the pod
is hostNetwork, so two instances can't share the node's ports). The kubelet
plugin's `RollingUpdate` option (pod UID via the downward API) registers a
UID-suffixed socket so the kubelet keeps a prepared node across the restart,
and an immutable pinned image with `imagePullPolicy: IfNotPresent` means the
restart involves no pull — the per-node window is a fast restart, and the
node-critical container can recover from cache during a registry outage.

## Security model

The DaemonSet is deliberately minimal in privilege:

- **`hostNetwork`** — required, and the *only* host namespace used. It is
  what lets the uevent listener see host device events. No `hostPID`, no
  `privileged`.
- **`NET_ADMIN`** — the single capability, for the netlink subscribe.
- **ValidatingAdmissionPolicy** — the driver's ServiceAccount has cluster-wide
  `resourceslices` write (it must, to publish), so a policy pins every write
  to (a) the `llmfit.ai` driver and (b) the writing pod's *own* node, read
  from the kubelet-issued bound token's `node-name` claim. A compromised
  node's driver cannot forge or delete another node's inventory.
- **`system-node-critical` + broad tolerations** — the driver must run on
  every accelerator node (routinely tainted) and survive eviction pressure,
  since eviction wedges the claim lifecycle while slices stay published.

## Packaging and release

llmfit (Rust) is a git submodule pinned to a release tag and compiled into
the image by a hermetic multi-stage Dockerfile — no host toolchain leaks in,
so a clean `git clone --recurse-submodules` reproduces the build. The runtime
image carries both binaries plus `curl` (the sidecar's liveness probe).

Releases are cut by **release-please** from conventional commits: merging the
release PR tags `vX.Y.Z`, which bumps the chart's `version`/`appVersion`,
builds the matching image tag, and publishes the chart to
`oci://ghcr.io/sympozium-ai/charts/llmfit-dra`. (A `RELEASE_TOKEN` PAT is
required for the tag push to trigger the build workflow — a `GITHUB_TOKEN`-
created tag does not.)

CI is tiered to keep per-push cost low: unit tests (`-race`) + chart lint + a
single-node e2e on every push; the 3-node multi-node e2e runs nightly, on
release tags, and on demand; docs-only changes skip CI.

## The end-to-end suite

`hack/scenarios.sh` is the executable specification — 15 scenarios covering
publish, shape, reconcile, CEL allocation → Running with CDI edits,
provenance, the CPU-only path, prepare-state lifecycle, driver-restart
resilience, `llmfit claim` generation, a Deployment + ResourceClaimTemplate,
cross-driver `matchAttribute` alignment, uevent hot-attach, vendor
coexistence, admission-policy enforcement, and real Vulkan compute in a
claimed pod. GPU assertions self-skip on GPU-less nodes; `make scenarios-cpu`
reproduces the CI mode anywhere.
