# Sympozium on llmfit-dra: from telemetry side-channel to scheduler-native placement

**Status:** Proposed (document-only — no implementation committed) · **Date:** 2026-07-03
**Decision sought:** none yet; this is the migration map. A deliberately open
question — whether llmfit-dra stays closed-source — shapes the design and is
addressed head-on in §6.

## 1. What sympozium does today (verified against the code)

Sympozium's model placement runs on a **side-channel** the POC doc predicted
DRA would replace. The verified inventory:

```
llmfit-daemon DaemonSet (charts/sympozium/templates/llmfit-daemonset.yaml)
  llmfit serve --host 0.0.0.0 :8787, host /proc /sys /dev mounts
   │  NATS llmfit.{system,fit,runtimes,installed}.{host}   (primary)
   │  REST :8787 /api/v1/system, /api/v1/models             (fallback poller)
   ▼
DensityCache — the "FitnessCache" (internal/controller/density_cache.go)
   in-memory, TTL 90s, instantiated INDEPENDENTLY in three binaries:
   controller (cmd/controller/main.go:264), apiserver (:115), webhook (:89)
   │
   ├─ ModelReconciler.reconcilePlacing (model_controller.go:129)
   │    BestNodeForModel(…,"good") → Model.Spec.NodeSelector =
   │    {kubernetes.io/hostname: <node>} + limits["nvidia.com/gpu"]  (:136, :929)
   │    cache empty? → spawn per-node probe pods, parse JSON logs (:148-338)
   ├─ DensityWatcher (live eviction: clears NodeSelector, re-Placing)
   ├─ ModelDensityValidator (admission: reject unfittable Models)
   ├─ DensityMetrics (Prometheus)
   └─ apiserver /api/v1/density/* (nodes, cost, simulate, catalog → UI)
```

Structural weaknesses this migration removes:

1. **Placement is hostname-pinning.** The controller re-implements a
   scheduler: it picks a node from cached telemetry and hard-pins via
   `NodeSelector`. Races (device claimed between cache update and pod
   start), no exclusivity (two Models can pin the same GPU), and the
   kube-scheduler never knows why.
2. **Three independent caches** of the same telemetry (controller,
   apiserver, webhook) with 90s TTLs — three ways to disagree, three NATS
   subscriptions to operate.
3. **The slow path spawns probe pods** per node when the cache is cold —
   fragile, slow, and privileged.
4. **GPU is `nvidia.com/gpu`** — nothing for AMD/Apple/NPUs, no memory or
   bandwidth semantics, no fit awareness at the scheduler level.
5. Eviction mutates `NodeSelector` — fighting the same race again.

Out of scope (verified orthogonal): the `node-probe` DaemonSet and its
`sympozium.ai/inference-*` annotations (host-inference discovery for agent
pinning, different subsystem) and the `skill-llmfit` SkillPack (agent
tooling).

## 2. What llmfit-dra replaces it with

| Sympozium mechanism | llmfit-dra native equivalent |
|---|---|
| NATS/REST telemetry → DensityCache | **ResourceSlices**: typed, watchable, per-device inventory in the API server — one shared source instead of three process-local caches |
| `BestNodeForModel` + NodeSelector pin | **ModelClaim** → same-named ResourceClaimTemplate → the *stock scheduler* places via CEL. Exclusive allocation, no races, no pinning |
| `nvidia.com/gpu` limit | Device request in the claim (vendor-neutral; CPU/NPU/unified-memory covered; vendor-driver coexistence via `vendorManaged` demotion) |
| ModelDensityValidator (admission reject) | ModelClaim's **`Satisfiable` condition** + shortfall message — same verdict, no webhook, and visible *before and after* admission |
| DensityWatcher eviction | Device `healthy` flips + (alpha) DRA device taints; claim delete → normal rescheduling |
| Probe-pod slow path | Deleted. Slices are always warm — the driver publishes on change and on a 60s floor |

The one thing slices deliberately do **not** carry is per-model fit (Tenet
3: physics inputs, not verdicts). Sympozium's UI endpoints that join the
model catalog to hardware (`/api/v1/density/{catalog,cost,simulate}`) are
handled in §5.

## 3. Migration phases

Designed so every phase is independently shippable, reversible, and
coexists with the current path. K8s < 1.34 clusters keep the legacy path
throughout (§4).

### Phase 0 — bundle, observe, change nothing
Ship the llmfit-dra chart as an **optional dependency** of the sympozium
chart (`llmfitDra.enabled: false` default). When enabled, slices publish
alongside the existing NATS flow; nothing consumes them yet. Operators can
`kubectl get resourceslices` and compare against `/api/v1/density/nodes`.
*Effort: chart plumbing only.*

### Phase 1 — read side: one source of truth
Put an interface in front of `DensityCache` and add a
**ResourceSlice-backed implementation** (one informer, shared semantics,
no TTL guesswork). Controller, apiserver, webhook, and metrics consume it
when slices exist; the NATS/poller feed stays as fallback. This deletes
the triple-cache inconsistency *before* touching placement.
Files: `density_cache.go` (interface extraction), `cmd/{controller,apiserver,webhook}/main.go`
wiring, `density_metrics.go`. *Effort: ~3-4 days + soak.*

### Phase 2 — placement: the load-bearing swap
`reconcilePlacing` stops choosing nodes. Instead:

1. Controller creates a **ModelClaim** named after the Model (same
   namespace, ownerRef) — `spec.model` from the Model's model reference,
   `minTps` from a new optional `ModelPlacement.minTps` (default keeps
   current behavior), `deviceClassName` derived from
   `ModelResources.GPU > 0 → gpu.llmfit.ai`.
2. `ensureDeployment` replaces `NodeSelector` + `nvidia.com/gpu`
   (model_controller.go:975, :924-930) with
   `resourceClaimTemplateName: <model-name>` — the same-name contract
   means no discovery step.
3. Status mapping: ModelClaim `Resolved/Satisfiable` → Model conditions;
   allocated node (from the stamped claim's allocation result) →
   `status.PlacedNode`; the shortfall message → `PlacementMessage` (a
   strict upgrade: today's message can't say *why* nothing fits).
4. The probe-pod slow path and `BestNodeForModel` placement call are
   deleted. The admission validator becomes a read of `Satisfiable`
   (or is dropped — the condition makes rejection-at-admission optional
   rather than necessary).
5. Eviction: DensityWatcher stops mutating NodeSelector; unhealthy
   devices flip `healthy=false` (slices) and the watcher's only job is
   deleting the pod/claim to trigger rescheduling — or, once
   DRADeviceTaints graduates, nothing at all.

*Effort: ~1-1.5 weeks including status/UX mapping and e2e. This is the
phase that pays: exclusivity, vendor-neutrality, and scheduler-visible
placement all arrive here.*

### Phase 3 — retire the side-channel
Remove the `llmfit-daemon` DaemonSet, `density_subscriber.go`,
`density_poller.go`, and the NATS subjects; drop the fallback branch from
Phase 1's interface. Keep this phase gated on a full release cycle of
Phase 2 telemetry parity (§5 resolved).

## 4. Constraints that shape the design

- **K8s floor:** DRA is GA at 1.34; sympozium supports older clusters.
  All DRA behavior is **runtime-detected** (does
  `resource.k8s.io/v1` serve? does the `modelclaims.llmfit.ai` CRD
  exist?) — never compile-time. Legacy path remains for pre-1.34.
- **No private code in the public repo (§6):** integration is entirely
  through API groups — `k8s.io/api/resource/v1` (upstream) and the
  `llmfit.ai` CRD via the dynamic client (group/version/kind strings).
  Sympozium never imports llmfit-dra Go packages.
- **RBAC:** controller adds `resourceclaims`/`resourceclaimtemplates`
  (via ModelClaim: only `modelclaims` CRUD + status read),
  `resourceslices` read. Chart + `+kubebuilder:rbac` markers.
- **Vendor coexistence:** on nodes where NVIDIA's driver owns allocation,
  llmfit devices are `vendorManaged`-demoted; sympozium Models needing
  those GPUs keep the `nvidia.com/gpu` path on such nodes (mixed-fleet
  reality; matchAttribute alignment is the eventual answer).

## 5. The catalog/cost/simulate gap (and its elegant fix)

`/api/v1/density/{catalog,cost,simulate}` join the **model catalog** to
node hardware — data slices don't carry. Three options:

a) Keep a thin llmfit REST DaemonSet just for the UI. (Defeats Phase 3.)
b) Teach the llmfit-dra sidecar to optionally listen on TCP. (Widens the
   driver's surface for a UI concern.)
c) **The apiserver runs the llmfit binary itself** with hardware
   *simulation* inputs taken from ResourceSlices: fit is a pure function
   of (specs, catalog), and llmfit already supports spec overrides
   (`--memory/--ram`, simulation mode). Slices provide per-node specs;
   the apiserver computes catalog/cost/simulate locally — zero per-node
   calls, no DaemonSet, always as fresh as the slices.

(c) is the recommendation; it also decouples UI freshness from node
round-trips. Prerequisite: llmfit CLI accepts a full spec-override set
(bandwidth/backend, not just memory) — small upstream addition.

## 6. The open-source question

llmfit-dra is private today; sympozium is public. The design makes that a
**distribution decision, not an architecture decision**:

- **Build-time:** zero coupling. Public sympozium compiles against
  upstream `resource.k8s.io` types and manipulates ModelClaims as
  unstructured objects. Nothing private is vendored, imported, or even
  named beyond API-group strings — the same way any operator integrates
  with, say, cert-manager CRDs without shipping cert-manager.
- **Run-time:** llmfit-dra is detected, never assumed. Absent → sympozium
  behaves exactly as today (NATS path). Present → placement upgrades.
  The public repo's docs can honestly say "optionally integrates with a
  DRA driver publishing `llmfit.ai` devices" without disclosing anything
  about the driver's internals.
- **If it stays closed:** distribution continues via private GHCR
  (`make pull-secret` pattern) to you/partners — effectively a
  proprietary "pro" placement engine under an open orchestrator. The
  draft-release flow and chart pullability checks already support this.
- **If it opens later:** flip `llmfitDra.enabled` default and move the
  chart dependency from "bring your own registry" to public GHCR. No
  code changes in sympozium either way.

The one thing to **avoid** until decided: putting llmfit-dra design
details, this document, or the ModelClaim schema rationale into the public
sympozium repo. Integration PRs there should reference the CRD as an
external API, nothing more.

## 7. Risks

- **Two placement engines during Phases 1-2** — mitigated by the
  runtime-detection flag being per-Model overridable
  (`placement.mode: dra|legacy|auto`), and by Phase 1 shipping read-only.
- **Slice→UI parity**: `/density/nodes` fields must map 1:1 from slice
  attributes before Phase 3 removes NATS; audit `density_handlers.go`
  field-by-field.
- **DensityWatcher semantics change**: today "degraded 30% → evict";
  post-migration health is boolean until llmfit publishes utilization
  (deliberately out of slices — Tenet: inventory, not metrics). Live
  *performance* eviction would need the NATS metrics path retained or a
  metrics-based Descheduler policy — flag this as the one capability
  without a 1:1 DRA equivalent.
- **Alpha dependencies**: none required (device taints optional).

## 8. Summary table

| Phase | Ships | Deletes | Risk |
|---|---|---|---|
| 0 | optional chart bundle | — | none |
| 1 | slice-backed cache behind interface | triple-cache drift | low |
| 2 | ModelClaim placement, Satisfiable→status | NodeSelector pinning, probe pods, nvidia.com/gpu, admission webhook (optional) | medium |
| 3 | — | llmfit-daemon, NATS subjects, poller | low (gated on §5c) |
