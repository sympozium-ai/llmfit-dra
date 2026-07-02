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

## Status

**Phase 1 — publish (done).** Devices are visible to the scheduler, Kueue,
and controllers; CEL allocation works against our attributes.

**Phase 2 — claimable (done).** The full path from model name to running
pod, with zero Sympozium components:

- **DeviceClasses** (`deploy/deviceclass.yaml`): `llmfit.ai` as the base
  class every claim targets, plus per-kind classes (`gpu.llmfit.ai`,
  `npu.llmfit.ai`, `cpu.llmfit.ai`) whose names give Kueue and
  ResourceQuota a countable vocabulary. Class selectors AND with claim
  selectors — the class guarantees an llmfit device of that kind, the
  claim adds the model-specific fit CEL.
- **Kubelet DRA plugin** (`internal/nodeplugin`): prepares allocated
  devices via CDI — one spec file per claim under `/var/run/cdi` injecting
  the device's `/dev` nodes (DRM render + card node, `/dev/kfd` for
  amdgpu, `/dev/accel/accelN` for NPUs) and env: `LLMFIT_DEVICE`,
  `LLMFIT_DEVICE_KIND`, `LLMFIT_RENDER_NODE`, plus per-device
  `LLMFIT_DEVICE_<NAME>=<node>` (survives multi-device CDI merges).
  `cpu0` prepares env-only, so the claim→Running loop works with no
  accelerator at all. The spec file doubles as prepare-state: restarts
  need no checkpoint, unprepare is "remove the file".
- **`llmfit claim`** generates the fit CEL from the model database (see
  below).

**Phase 3 — alignment & liveness (done, minus vendor-hardware items).**
Shipped: the standardized `resource.kubernetes.io/pcieRoot` attribute +
`matchAttribute` alignment (scenario 11); honest health — `healthy` is
computed per probe cycle (kernel driver bound, no uncorrectable RAS
errors) with a `healthReason` attribute when false; **event-driven
re-probe** — a netlink uevent listener (drm/accel/pci subsystems,
`hostNetwork`) triggers an immediate walk on hot-attach, driver
bind/unbind, and error events, with the ticker as reconciliation floor
(scenario 12); and `--publish-taints`, which taints unhealthy devices
`NoSchedule` (off by default — needs the alpha `DRADeviceTaints` gate).
Blocked on hardware we don't have: vendor event streams (XID/DCGM — the
vendor DRA driver's job on nodes where one runs) and live cross-driver
`matchAttribute` against NVIDIA/Neuron (needs a mixed node).

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

One ResourceSlice per node (pool = node name). PCI devices are named by
PCI address — `gpu-0000-c3-00-0`, `npu-0000-c4-00-1` — because DRA
allocations and kubelet-plugin prepare join on device *names*, so a name
must identify the same silicon across reboots, hot-remove, and driver
reloads (an enumeration counter like `gpu0` does not). The CPU fallback
stays `cpu0`. Descriptive identity still lives in attributes.

| Attribute (`llmfit.ai/…`) | Type | Example |
|---|---|---|
| `kind` | string | `gpu` \| `npu` \| `cpu` |
| `vendor` | string | `intel`, `nvidia`, `amd`, `cpu` |
| `model` | string | `Intel Arc Graphics 140V` (from index) |
| `driver` | string | `xe`, `intel_vpu` |
| `pciAddress` / `pcieRoot` | string | `0000:00:02.0` / `pci0000:00` — correlation keys to vendor DRA drivers |
| `resource.kubernetes.io/pcieRoot` | string | same value, standardized spelling — enables cross-driver `constraints.matchAttribute` |
| `memoryBandwidthGBs` | int | `136` — the number tok/s physics hangs off; **index-sourced, not OS-discoverable** |
| `unifiedMemory` | bool | `true` on iGPUs/Apple-class devices |
| `indexed` | bool | whether the capability index recognized the PCI ID |
| `source` | string | capability provenance: `llmfit` \| `index` \| `probe` |
| `backend` | string | llmfit inference backend: `Vulkan`, `CUDA`, `ROCm`, `SYCL`, `Metal` |
| `healthy` | bool | computed per probe cycle: kernel driver bound, no uncorrectable RAS errors |
| `healthReason` | string | only when unhealthy: `driverUnbound` \| `uncorrectableEcc` |
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

## Generating claims: `llmfit claim`

Nobody should compute fit constants by hand. The llmfit CLI (which the
driver image ships) resolves them from its model database and emits the
claim as plain YAML on stdout — pipe it straight to kubectl. No local
binary needed; exec through the DaemonSet:

```sh
kubectl -n llmfit-dra exec ds/llmfit-dra -- \
  llmfit claim Qwen/Qwen2.5-7B --min-tps 20 | kubectl apply -f -
```

What comes out (constants inlined from the model database, with provenance):

```yaml
# Generated by llmfit claim — do not compute these constants by hand.
# model:  Qwen/Qwen2.5-7B (7.6B params, Q4_K_M ≈ 4.4 GB weights)
# fit:    tok/s ≈ bandwidth × 55% / 4.4 GB  ⇒  bandwidth ≥ 161 GB/s for ≥ 20 tok/s
# memory: ≥ 5 Gi (weights + KV/runtime headroom)
apiVersion: resource.k8s.io/v1
kind: ResourceClaim
metadata:
  name: qwen-qwen2-5-7b-fit
spec:
  devices:
    requests:
      - name: model
        exactly:
          deviceClassName: llmfit.ai
          selectors:
            - cel:
                expression: >-
                  device.capacity['llmfit.ai'].memory.compareTo(quantity('5Gi')) >= 0 &&
                  device.attributes['llmfit.ai'].memoryBandwidthGBs >= 161 &&
                  device.attributes['llmfit.ai'].healthy
```

A pod references the claim by name and the stock kube-scheduler places it
on a node whose accelerator satisfies the physics; the kubelet plugin then
injects the device (scenario 9 asserts this exact flow end-to-end):

```yaml
spec:
  resourceClaims:
    - name: model
      resourceClaimName: qwen-qwen2-5-7b-fit
  containers:
    - name: main
      resources:
        claims:
          - name: model
```

Useful flags: `--template` emits a ResourceClaimTemplate (for Deployment pod
templates), `--quant Q8_0` overrides the database entry's quantization,
`--efficiency`/`--device-class`/`--name` tune the rest. Ambiguous model
names fail with suggestions — names match exactly or by unique substring.

## How llmfit is consumed

Two couplings, one at runtime and one at build time:

**Runtime.** The Go driver does not link llmfit — it shells out to
`llmfit --json system` (path from `LLMFIT_BIN`, baked into the image as
`/usr/local/bin/llmfit`) on every probe cycle and parses the JSON
(`internal/llmfit`). If the exec fails or llmfit doesn't model a device class,
the publisher degrades to the embedded index — the `source` attribute on every
device tells you which path was taken.

**Build.** llmfit is a **git submodule** at `third_party/llmfit`, pinned to
the `v0.9.35` release tag — the first release with `memory_bandwidth_gbps`
in `system --json`, the Strix Halo bandwidth entries, and the `claim`
subcommand. The Dockerfile is
hermetic: a `rust:1-slim-bookworm` stage compiles the submodule with
`cargo build --release -p llmfit`, and the runtime stage
(`debian:bookworm-slim` + pciutils) copies in both binaries. No host toolchain
leaks into the image, so builds are reproducible from a clean
`git clone --recurse-submodules`.

To bump the pin: `cd third_party/llmfit && git fetch --tags && git checkout <tag>`,
commit the submodule update, and CI rebuilds against it.

**CI / images.** `.github/workflows/build.yml` runs vet + unit tests, an
**e2e job** (kind v1.35 cluster on the runner, full scenario suite via the
cpu0 fallback device — the GPU assertions self-skip), then
builds and pushes `ghcr.io/sympozium-ai/llmfit-dra` on every push to `main`
(tags: `latest`, `main`, `sha-<short>`) and on `v*` tags (semver). PRs build
without pushing. linux/amd64 only for now — the llmfit Rust stage under QEMU
makes arm64 impractical; revisit with native arm64 runners.

## Layout

```
cmd/llmfit-dra/        main: flags, kube client, probe loop, plugin wiring
internal/probe/        device tree walk (sysfs/procfs, root-parameterized for tests):
                       identity, /dev nodes, driver binding, RAS health
internal/index/        embedded capability index (data.json)
internal/llmfit/       exec + parse `llmfit --json system` (capability source)
internal/publisher/    probe ⋈ index ⋈ llmfit → resource.k8s.io/v1 Devices; resourceslice helper
internal/nodeplugin/   kubelet DRA plugin: NodePrepareResources → CDI specs in /var/run/cdi
deploy/                namespace, RBAC, DeviceClasses, DaemonSet
hack/scenarios.sh      live-cluster scenario suite (see Scenarios)
hack/scenarios-cpu.sh  the same suite forced through the cpu0-only path (reproduces CI)
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
4. **Consume** — the shipped DeviceClasses exist; a ResourceClaim with a CEL fit expression gets **allocated by the stock kube-scheduler** and the kubelet plugin prepares it: the pod reaches Running with the CDI edits visible inside.
5. **Provenance** — `source=llmfit` on devices llmfit assessed.
6. **CPU path** — a `cpu.llmfit.ai` claim prepares env-only; the whole claim→Running loop works with no accelerator (this is what CI exercises).
7. **Lifecycle** — the per-claim CDI spec exists while the pod runs, unprepare removes it.
8. **Restart** — running consumers survive a driver restart; the new instance prepares fresh claims and unprepares the old instance's.
9. **Claim generation** — `llmfit claim Qwen/Qwen2.5-7B --min-tps 20 | kubectl apply -f -` (generator runs from the image) allocates and runs.
10. **Deployment** — the Phase 2 exit criterion verbatim: a vanilla Deployment consuming a `llmfit claim --template`-generated ResourceClaimTemplate lands on the fit device and runs; the per-pod claim is garbage-collected on delete.
11. **Alignment** — one claim requests gpu + npu constrained by `matchAttribute: resource.kubernetes.io/pcieRoot`; both allocate and both prepare into one pod (multi-device CDI merge, per-device `LLMFIT_DEVICE_<NAME>` env).

GPU-specific assertions self-skip on nodes without a **fit-capable** `gpu0`
(one with a bandwidth attribute — virtual display adapters on CI runners
don't count), so the suite is identical on the dev rig and in CI.
`make scenarios-cpu` reproduces the CI run on any cluster by hiding sysfs
from the probe (cpu0-only inventory), restoring the DaemonSet afterwards.

## Roadmap

- **Hardware-blocked Phase 3 tails**: live cross-driver `matchAttribute` against a vendor DRA driver (needs a mixed NVIDIA/Neuron node); vendor event streams (XID/DCGM) as an additional health source.
- **Index as artifact**: extract `internal/index/data.json` into a versioned dataset other drivers can vendor.
- **Image base**: optionally consume llmfit's release image (`COPY --from`) instead of compiling the submodule — faster CI, at the cost of pinning an image digest rather than a source SHA.

Design doc: *POC — llmfit as a DRA ResourceSlice Publisher* (Obsidian, sympozium vault).
