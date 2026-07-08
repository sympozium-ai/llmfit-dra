# Implementation plan: Sympozium on llmfit-dra + native ModelClaims

**Status:** Planned · **Date:** 2026-07-08
**Parent design:** `sympozium-integration.md` (2026-07-03). That doc is the *why* and the phase map; this doc is the work breakdown, the file-level touch list, and one scope addition: **exposing claims from inside Sympozium itself** (Ensemble personas and agent-facing tools), which the parent doc did not cover.

## 0. Scope

Two deliverables:

1. **Replace the llmfit DaemonSet side-channel** (NATS/REST → DensityCache → hostname pinning) with llmfit-dra ResourceSlices + ModelClaim placement. Parent doc Phases 0–3, unchanged in intent, detailed here.
2. **Make claims a native Sympozium surface**: a Sympozium user (or agent) never writes `llmfit.ai` YAML. Claims are materialized from the `Model` CRD, from Ensemble persona requirements, and (policy-gated) from an agent tool call. New Phase 2.5.

Non-goals: multi-model claims, WorkflowClaim/stage graphs (tracked separately as the llmfit-dra M3 / StageClaim design), performance-based eviction (see §7 — the NATS metrics path is deliberately retained past Phase 3).

## 1. Ground truth (verified 2026-07-08)

- Sympozium placement today: `reconcilePlacing` (model_controller.go:129) → `DensityCache.BestNodeForModel(query, "good")` (:133) → NodeSelector hostname pin + `nvidia.com/gpu` limit; probe-pod slow path (:148-338); `DensityCache` instantiated independently in controller, apiserver, webhook; DensityWatcher mutates NodeSelector on degradation.
- `ModelPlacement` (model_types.go:163) has `Mode: manual|auto` only. Status carries `PlacementScore`/`PlacementMessage`.
- llmfit-dra v0.2.7: ModelClaim M1+M2 shipped (CRD, controller, `Satisfiable` with shortfall + allocation-awareness), same-name ResourceClaimTemplate contract, vendorManaged coexistence, Helm/OCI packaging. No serve-API resolution transport yet (M3) — not a blocker.
- Ensemble `AgentConfigSpec` overrides are provider-level strings (`Model`, `Provider`, `BaseURL`) — there is **no reference from a persona to a Sympozium `Model` CR** today. Agents point at a Model's endpoint only by hand-written BaseURL. Phase 2.5 fixes this.

## 2. Phase 0 — bundle and observe (chart plumbing, ~1–2 days)

- Add llmfit-dra as an **optional Helm dependency** of `charts/sympozium` (`llmfitDra.enabled: false`), pullable from private GHCR via the existing `make pull-secret` pattern. Alternatively (decision): document side-by-side install rather than subchart, to keep the public chart free of private registry references — leaning **side-by-side + values-level detection** for §6 hygiene.
- New package `internal/dra/detect.go`: runtime capability probe — does the API server serve `resource.k8s.io/v1`? does `modelclaims.llmfit.ai` exist? Cached with re-check on interval. Everything downstream branches on this; nothing is compile-time.
- Acceptance: on a DRA cluster with the driver installed, `kubectl get resourceslices` and `/api/v1/density/nodes` can be compared side by side; zero behavior change.

## 3. Phase 1 — one source of truth for reads (~1 week + soak)

- Extract interface from `density_cache.go`:
  ```go
  type DensityProvider interface {
      BestNodeForModel(q ModelQuery, minFit string) (node string, score int, msg string)
      Nodes() []NodeDensity
      // narrow: only what controller/apiserver/webhook actually call
  }
  ```
- New `SliceDensityProvider` (`internal/controller/slice_density_provider.go`): one shared ResourceSlice informer; maps `llmfit.ai` attributes (memory capacity, `memoryBandwidthGBs`, `unifiedMemory`, `healthy`, `vendorManaged`, `source`) into the existing `NodeDensity` shape. Fit scoring reuses llmfit-core semantics via the same claim-physics math (pure function of specs + catalog — no per-node calls).
- Wiring: `cmd/controller/main.go:264`, `cmd/apiserver/main.go:115`, `cmd/webhook/main.go:89` choose provider by `dra.Detect()`; NATS/poller path remains as fallback implementation behind the same interface. This deletes the triple-cache drift *before* touching placement.
- `density_metrics.go` consumes the interface; add `sympozium_density_source{provider="slices|nats"}` so parity is observable.
- Acceptance: with driver present, all three binaries report identical node inventory from slices; NATS subscription count drops to zero in slice mode; soak one release.

## 4. Phase 2 — the load-bearing swap: Model → ModelClaim (~1.5 weeks)

API (`api/v1alpha1/model_types.go`):
- `ModelPlacement.Mode`: add `dra` (and make `auto` mean "dra if detected, else legacy"). Per-Model override is the rollback lever.
- Add `MinTps *int32` (optional; nil keeps today's "fits at good quality" semantics — the claim is emitted memory-floor-only, matching ModelClaim's satisfiable-without-bandwidth mode for CPU classes).
- Status: add `ClaimName`, map ModelClaim `Resolved`/`Satisfiable` → Model conditions; keep `PlacementMessage` (now carries the exact shortfall, e.g. "closest device gpu-0000-c3-00-0 (node strix): bandwidth 256 < 640" — a strict UX upgrade).

Controller (`internal/controller/model_controller.go`):
- `reconcilePlacing` DRA branch: ensure a **ModelClaim** (unstructured, `llmfit.ai/v1alpha1`, same name/namespace, ownerRef → Model) with `spec.model` from the Model's reference, `spec.minTps` from placement. **No llmfit-dra Go imports** — dynamic client + GVK strings only (§6 of parent doc).
- `ensureDeployment` (:924-975 region): replace NodeSelector pin + `nvidia.com/gpu` limit with pod-spec `resourceClaims: [{name: model, resourceClaimTemplateName: <model-name>}]` + container `claims` entry. Same-name contract means no discovery step.
- `PlacedNode`: read the stamped ResourceClaim's allocation result once the pod schedules; surface node + device (PCI address) in status.
- Delete (DRA mode): probe-pod slow path (:148-338), `BestNodeForModel` call, admission reject in `model_density_validator.go` (becomes a warn based on `Satisfiable=False`; rejection is optional now that unsatisfiability is visible pre-pod).
- `density_watcher.go`: in DRA mode its only job is deleting the pod when a device goes `healthy=false` (claim release → normal rescheduling). NodeSelector mutation code is legacy-path only.
- Vendor coexistence wrinkle (mixed fleets): on nodes where NVIDIA's driver owns the GPUs, llmfit devices are `vendorManaged`-demoted and excluded from the shipped DeviceClasses. Models that must land there stay on the legacy `nvidia.com/gpu` path — `placement.mode: legacy` per Model, documented; `matchAttribute` fitness-companion alignment is the eventual fix, out of scope here.

RBAC: controller gains `modelclaims` CRUD + status read, `resourceclaims`/`resourceclaimtemplates` read, `resourceslices` read (chart `rbac.yaml` + kubebuilder markers).

Acceptance (e2e, kind ≥1.34 + driver): Model with `placement.mode: dra` schedules via claim with zero NodeSelector; two Models cannot double-book one device (the race that exists today); unsatisfiable Model reports the shortfall in `PlacementMessage` before any pod exists; `mode: legacy` still works on the same cluster.

## 5. Phase 2.5 — claims as a native Sympozium surface (new scope, ~1–2 weeks)

This is the piece the parent doc didn't cover: making the claim the *product surface*, not an implementation detail.

**(a) Ensemble personas claim models.** Add to `AgentConfigSpec` (ensemble_types.go):
```yaml
modelClaim:            # optional; mutually exclusive with provider/baseURL overrides
  model: qwen3.6-30b-a3b
  minTps: 20
```
The Ensemble controller materializes (or reuses, keyed by `{model, minTps}` hash) a Sympozium `Model` CR with `placement.mode: dra`, waits for Ready, and injects the Model's OpenAI-compatible service URL as that persona's `BaseURL`. Result: **one Ensemble can declare a planner on fast silicon and a verifier at 5 tok/s on whatever is cheap, with zero node labels** — the heterogeneous-recursion pattern as three lines of YAML per persona. Owner-ref through the Ensemble cascade; shared Models ref-counted via a finalizer or ownerRef-per-consumer annotation (decide during implementation; ref-count annotation is simpler and survives one persona being deleted).

**(b) API surface.** apiserver:
- `GET /api/v1/claims` (list ModelClaims with `Satisfiable`, candidate counts, holder), `GET /api/v1/claims/{name}` — thin reads over the dynamic client, policy-scoped by namespace.
- `/api/v1/density/{catalog,cost,simulate}` re-implemented per parent-doc §5(c): apiserver runs the llmfit binary with **spec overrides fed from ResourceSlices** (fit is a pure function of specs + catalog). Prerequisite upstream: llmfit CLI accepts full spec-override set (`--bandwidth`, `--backend`, not just memory) — small llmfit change, file upstream first.
- UI placement panel: show claim status + shortfall instead of `PlacementScore`.

**(c) Agent-facing claims (policy-gated).** New built-in tool / MCP tool `request_model`:
```json
{"model": "...", "minTps": 20, "ttlMinutes": 60}
```
→ creates the Model CR (hence claim) in the agent's namespace, returns the endpoint when Ready, TTL-reaped by a janitor. Gated three ways: `SympoziumPolicy` tool allowlist, the admission webhook (namespace quota on agent-created Models), and Membrane `TokenBudget` (a claimed model is spend). This is the "workloads claim outcomes" story made agentic — an agent that decides mid-task it needs a bigger model *claims one*, and the scheduler either satisfies it or the tool returns the exact shortfall for the agent to reason about. Ship behind a feature flag; default off.

Acceptance: e2e where an Ensemble declares two personas with different `modelClaim`s and both place correctly on a two-tier kind cluster; an agent granted `request_model` provisions and uses a model, and is refused by policy when the namespace quota is hit.

## 6. Phase 3 — retire the side-channel (~1 week, gated)

- Remove `llmfit-daemonset.yaml`, `density_subscriber.go`, `density_poller.go`, NATS `llmfit.*` subjects; drop the fallback branch from the Phase 1 interface **except** the metrics feed (§7).
- Gate: one full release of Phase 2 telemetry parity, `density_handlers.go` field-by-field audit signed off, §5(c) llmfit CLI change shipped.

## 7. Deliberately retained: the performance-eviction gap

"Degraded 30% → evict" has no DRA equivalent (slices are inventory, not metrics — Tenet 3). Decision: **retain the NATS metrics path solely as the DensityWatcher's degradation feed** until a metrics-driven descheduler exists (that descheduler is the first "adaptation plane" workstream and is out of scope here). Phase 3 therefore removes NATS as a *placement* input, not as a *metrics* input. Revisit when llmfit-dra grows a telemetry-correction loop.

## 8. Sequencing, effort, risks

| Phase | Effort | Rollback lever |
|---|---|---|
| 0 bundle + detect | 1–2 d | values flag |
| 1 slice-backed provider | ~1 w + soak | interface swap back to NATS impl |
| 2 ModelClaim placement | ~1.5 w | per-Model `placement.mode: legacy` |
| 2.5 native claim surfaces | ~1–2 w | feature flags per surface (a/b/c independent) |
| 3 retire side-channel | ~1 w | gated; metrics feed retained |

Total ≈ 5–6 focused weeks; Phases 0–2 are the credible pre-September target, 2.5(a) is the demo that matters (heterogeneous Ensemble on mixed silicon).

Risks (beyond parent doc §7): Ensemble→Model ref-counting lifecycle bugs (mitigate: e2e for shared-model deletion); agent-created Models as a cost/abuse vector (mitigate: default-off, quota + TokenBudget + TTL); dynamic-client schema drift if ModelClaim CRD evolves (mitigate: version-pin the GVK, tolerate unknown fields); kind-with-DRA CI matrix addition (K8s ≥1.34 image).

## 9. Decision log (to resolve before Phase 0 merge)

1. Subchart vs side-by-side install for the private driver → leaning side-by-side.
2. `minTps` default for `auto` placements → nil (memory-floor-only) to preserve current semantics.
3. Shared-Model ref-counting mechanism in 2.5(a) → annotation ref-count vs multi-ownerRef.
4. Whether `request_model` ships in the same milestone or trails 2.5(a)/(b) by one release.

## 10. Addendum (2026-07-08): open source + positioning consequences

llmfit-dra is now **public**, which retires two constraints in this plan and
the parent design's §6:

- Sympozium may import the typed API module
  (`github.com/sympozium-ai/llmfit-dra/api`, shipped in v0.3.0) directly —
  the unstructured/GVK-strings indirection in Phase 2 is no longer required.
  Amend Phase 2 step 1 accordingly.
- The chart becomes a normal public dependency
  (`oci://ghcr.io/sympozium-ai/charts/llmfit-dra`); the pull-secret pattern
  in Phase 0 applies only to pre-release testing.

Positioning docs now pin the boundary on both sides
(`sympozium/docs/positioning.md`, llmfit-dra README "Positioning"). Two
consequences fold into **Phase 3** as first-class work items rather than
afterthoughts:

- **node-probe leaves Sympozium.** Host-inference discovery is capability
  inventory (supply side). Migration shape: publish discovered endpoints as
  attributes on the node's llmfit.ai devices (or a companion slice), retire
  the `sympozium.ai/inference-*` node annotations, keep the reverse-proxy
  concern in Sympozium (it is an agent-facing consumption feature, not
  inventory).
- **The density dashboard is relabelled, not removed.** Post-Phase-1 it
  reads only from ResourceSlices; the UI should present it as "cluster
  capability (via llmfit-dra)" so no user re-learns it as a Sympozium
  subsystem.
