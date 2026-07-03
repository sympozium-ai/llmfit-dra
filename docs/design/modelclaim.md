# ModelClaim: "run Qwen3.6" as a Kubernetes object

**Status:** Proposed · **Author:** Alex Jones · **Date:** 2026-07-03
**Decision sought:** approve the `ModelClaim` v1alpha1 API + controller (Phase M1)

## Motivation

Every other DRA driver — NVIDIA, Intel, AMD — selects devices by **spec
sheet**: a memory quantity, a product name, a MIG profile. The user has to
already know that Qwen3.6-30B needs ~18 GiB at Q4 and a ~160 GB/s bandwidth
floor for 20 tok/s, and hand-translate that into attribute CEL. llmfit-dra
is the only driver that *owns that translation*: the model database and the
fit physics ship in the driver image. Requesting hardware **by model
capability** is our differentiator, and today it's buried behind an
imperative pipe:

```sh
kubectl -n llmfit-dra exec ds/llmfit-dra -- \
  llmfit claim Qwen/Qwen3.6-30B --min-tps 20 | kubectl apply -f -
```

That works, but it isn't declarative, isn't GitOps-able, has no status, no
validation, and no answer to "could this cluster *ever* run this model?".
`ModelClaim` makes the model request a first-class, reconciled object:

```yaml
apiVersion: llmfit.ai/v1alpha1
kind: ModelClaim
metadata:
  name: qwen36
spec:
  model: Qwen/Qwen3.6-30B-A3B
  minTps: 20
```

…and a pod consumes it exactly like any DRA workload:

```yaml
spec:
  resourceClaims:
    - name: model
      resourceClaimTemplateName: qwen36   # ← same name, emitted by the controller
```

## The constraint that shapes everything

Two Kubernetes facts are load-bearing and non-negotiable:

1. **The scheduler evaluates claim CEL against a candidate device's own
   attributes only.** It cannot consult a model database mid-scheduling. So
   "fits Qwen3.6 at 20 tok/s" must be reduced to literal numbers
   (`memory >= 18Gi && memoryBandwidthGBs >= 160`) *before* scheduling.
   A DeviceClass selector is fixed CEL with no claim-side parameters, and
   `spec.devices.config` reaches the driver only at *prepare* time — after
   placement. There is no in-API way to parameterize selection by model.
2. **Pods bind to devices through fixed core-API fields**
   (`pod.spec.resourceClaims[].resourceClaimName|resourceClaimTemplateName`).
   A custom kind cannot slot into that binding.

Together these dictate the design: `ModelClaim` is a **claim-template
generator** — a controller resolves the model to fit bounds ahead of time
and materializes a real `ResourceClaimTemplate` that pods reference by
name. Everything else in this doc is detail on making that translation
observable, validated, and safe.

## Tenets (inherited, plus one)

The POC tenets hold: llmfit is probe + index; the scheduler is the
placement engine; physics inputs, not verdicts; additive, not
rip-and-replace. One addition:

5. **llmfit-dra is never in the serving path.** `ModelClaim` emits a
   ResourceClaimTemplate and status — it does not create Deployments,
   Services, or pods. Serving orchestration belongs to the consumer
   (Sympozium's Model controller, a Helm chart, a human). The moment this
   controller creates a workload, we've become a mini-operator and broken
   the README's promise.

## API (v1alpha1)

```yaml
apiVersion: llmfit.ai/v1alpha1
kind: ModelClaim
metadata:
  name: qwen36
  namespace: team-a
spec:
  # Catalog name or HuggingFace repo id. Resolved against the llmfit model
  # database (embedded catalog + update cache + custom_models.json overlay).
  model: Qwen/Qwen3.6-30B-A3B          # required

  minTps: 20                            # optional, default 20; CEL: > 0
  quant: Q4_K_M                         # optional; default = catalog quant
  efficiencyPct: 55                     # optional, default 55; CEL: 1..100

  # DeviceClass the emitted request targets. Default llmfit.ai (any kind);
  # gpu.llmfit.ai to refuse CPU fallback, etc.
  deviceClassName: llmfit.ai            # optional

  # Escape hatch: extra CEL ANDed onto the generated selector, e.g. pinning
  # unifiedMemory or a vendor. Never replaces the generated physics.
  extraSelectors: []                    # optional, []string of CEL

status:
  observedGeneration: 3
  resolved:
    memoryGi: 18                        # device memory floor
    minBandwidthGBs: 160                # bandwidth floor for minTps
    quant: Q4_K_M
    weightsGb: "17.6"
    resolverVersion: "0.9.36"           # llmfit binary that computed this
  templateRef:
    name: qwen36                        # always == metadata.name
  candidates:                           # satisfiability snapshot
    devices: 2
    nodes: 1
  conditions:
    - type: Resolved                    # model found, bounds computed
      status: "True"
    - type: Satisfiable                 # ≥1 published device meets the bounds
      status: "True"
      message: "2 devices on 1 node satisfy memory>=18Gi, bandwidth>=160"
```

Kind name: `ModelClaim` — deliberately echoes both `ResourceClaim` (what it
becomes) and `llmfit claim` (its imperative twin). Namespaced, because the
emitted ResourceClaimTemplate is namespaced. Short name: `mclaim`.

### Validation — no webhooks in v1alpha1

Structural validation lives entirely in the CRD:

- OpenAPI schema types + required `model`.
- `x-kubernetes-validations` CEL rules: `minTps > 0`,
  `efficiencyPct in 1..100`, `model != ""`.

Semantic validation ("is this model in the database?") requires the
resolver, so it is reported **asynchronously** via `Resolved=False` with
reason `ModelNotFound` / `Ambiguous` (plus a Warning Event, so
`kubectl describe modelclaim` explains itself; the message includes
near-miss suggestions from llmfit's search). This keeps v1alpha1 entirely
free of webhook/cert infrastructure — the cost that sank the
mutating-webhook alternative.

## Semantics

### Binding: emit a ResourceClaimTemplate, same name, owned

For each `ModelClaim` the controller reconciles exactly one
`ResourceClaimTemplate` in the same namespace, **same name**, with an
ownerReference for garbage collection. Emitting a *template* (not a claim)
is the load-bearing choice:

- Pods reference it by a name they already know (`metadata.name`) — the
  indirection cost of the CRD collapses to zero.
- The platform stamps a fresh per-pod ResourceClaim from the template at
  pod admission and GCs it with the pod — the controller never manages
  per-pod claim lifecycle, finalizers, or orphan cleanup.
- Deployments/Jobs work naturally (each replica gets its own claim).

The generated template is byte-equivalent to `llmfit claim --template`
output: the guarded CEL over `device.capacity['llmfit.ai'].memory`,
`memoryBandwidthGBs`, and `healthy`, with provenance comments, ANDed with
any `extraSelectors`.

### Re-resolution and drift

The controller re-renders the template when:

1. `spec` changes (tracked by `observedGeneration`);
2. the **resolver version changes** — on controller upgrade the model DB
   may have moved; all ModelClaims re-resolve and `status.resolved` (and
   templates) update;
3. periodic resync (informer default) as a floor.

**Drift contract, stated plainly:** allocated ResourceClaims are immutable
— re-resolution never moves running pods. New pods stamped after an update
get the new bounds. This mirrors how a Deployment rollout treats old
ReplicaSets and is a feature, not a bug; the doc for users says exactly
this sentence.

If the ResourceClaimTemplate API rejects in-place spec updates
(templates are immutable in some API versions — implementation must
verify), the controller deletes and recreates under the same name;
already-stamped claims are unaffected.

### Failure behavior: never break a working template

- Resolver exec fails transiently → keep the last-good template, set
  `Resolved=True` with a `Degraded` reason on a new condition, log, retry
  with backoff. A model-DB hiccup must not cascade into scheduling
  failures for new pods.
- Model removed from DB on upgrade → keep last-good template, flip
  `Resolved=False (ModelNotFound)` so it's visible, emit an Event. Never
  delete the template out from under users.

### Satisfiability: the killer status feature

The controller already has cluster-wide read access to ResourceSlices
(coexistence logic). Because the generated constraint is a known
inequality — not arbitrary CEL — the controller can evaluate it directly
against every published device (memory capacity, `memoryBandwidthGBs`,
`healthy`) without embedding a CEL engine:

- `Satisfiable=True`, `candidates: {devices: N, nodes: M}` — and quota
  planning gets a real number.
- `Satisfiable=False` with the **best shortfall**:
  `"closest device gpu-0000-c3-00-0 (node strix): bandwidth 256 < 640"` —
  the exact answer to "why is my pod Pending", *before* a pod exists.

Refreshes on slice changes via the informer. Advisory only: it never
blocks template creation (clusters autoscale; a false "unsatisfiable"
must not gate anything).

## Resolution engine

The controller execs the llmfit binary **already in the image**:

```
llmfit claim <model> --min-tps N [--quant Q] [--efficiency E] --format json
```

**The single upstream llmfit change (M0):** a `--format json` flag on the
claim subcommand emitting `fit_bounds` + identity as JSON:

```json
{"model": "Qwen/Qwen3.6-30B-A3B", "quant": "Q4_K_M", "weightsGb": 17.6,
 "memoryGi": 18, "minBandwidthGBs": 160, "claimName": "qwen-qwen3-6-30b-a3b-fit",
 "resolverVersion": "0.9.36"}
```

Everything else (`fit_bounds`, `render`, guarded CEL) exists as of llmfit
v0.9.35/v0.9.36. Go renders the template from the JSON rather than
scraping YAML.

Two synergies worth naming:

- **Private models:** llmfit v0.9.36's `custom_models.json` overlay means a
  ConfigMap mounted at the data dir (or `LLMFIT_CUSTOM_MODELS`) lets a
  cluster define in-house models that `ModelClaim` can resolve — no llmfit
  release needed.
- **Future transport:** the AF_UNIX serve API could grow
  `GET /api/v1/claim?model=…` and the controller could consume it like the
  detector does, with the same degradation chain. Not needed for M1 —
  exec-in-image is simpler and the controller pod always ships the binary.

## Controller architecture

A separate **Deployment** (1 replica, leader election deferred), same
image, new entrypoint mode (`llmfit-dra --mode=controller` or subcommand).
Not in the DaemonSet: this is cluster-scoped reconciliation, not per-node
work, and the DaemonSet's host privileges (hostNetwork, NET_ADMIN for
uevents) must not leak into a component that only talks to the API server.

| RBAC | Verbs |
|------|-------|
| `modelclaims`, `modelclaims/status` (llmfit.ai) | get/list/watch, update/patch status |
| `resourceclaimtemplates` (resource.k8s.io) | get/list/watch/create/update/delete |
| `resourceslices` (resource.k8s.io) | get/list/watch (already held) |
| `events` | create/patch |

Watches: ModelClaims (primary), owned ResourceClaimTemplates (repair
drift/deletion), ResourceSlices (Satisfiable refresh).

Availability model: if the controller is down, existing templates keep
working — pods schedule normally. Only new/edited ModelClaims wait. Nothing
sits in the scheduling or pod-admission hot path. This is the decisive
robustness advantage over the mutating-webhook design.

## What this replaces (alternatives considered)

| | Verdict |
|---|---|
| **A. DeviceClass per model** | Rejected as a general mechanism: DeviceClasses are cluster-scoped shared vocabulary; one-per-model at catalog scale (5k models) is namespace pollution and reconcile churn (though not a scheduling-perf problem — claims name one class; lookup is O(1)). Survives only as an *optional, capped (~dozen), platform-curated* convenience for flagship families — and as the quota dimension (see below). **Explicit non-goal: classes derived from the catalog.** |
| **B. Mutating webhook on ResourceClaim** | Superseded by the CRD: same resolution, but the webhook has no durable object, no status/satisfiability story, weaker validation UX (annotations aren't schema'd), and brings cert/HA infrastructure into the *claim-creation* path with a nasty `failurePolicy` dilemma. The CRD keeps admission untouched. |
| **C. Per-model fit attributes on devices** | Rejected: models × devices cardinality, ~32-attribute cap per device, churn on every DB update. (Tenet 3 already forbade it.) |
| **D. Sympozium Model CRD** | Not a competitor — different altitude. Sympozium's Model controller orchestrates *serving*; `ModelClaim` is the hardware-claim layer it can build on. Positioning: Sympozium may generate ModelClaims (or keep generating raw claims); `ModelClaim` is the zero-Sympozium declarative path, as the CLI is the zero-Sympozium imperative path. |

## Interactions

- **Quota:** ResourceQuota/Kueue count by DeviceClass, and `ModelClaim`
  deliberately does not create classes — so *per-model quota* is out of
  scope for v1alpha1. Where model-level quota matters, the curated
  flagship classes (Option A residue) remain the mechanism; a ModelClaim
  can target one via `spec.deviceClassName`.
- **Vendor coexistence:** unchanged — the emitted CEL selects only
  llmfit.ai devices; `vendorManaged` demotion applies as today.
- **VAP:** the existing ValidatingAdmissionPolicy pins ResourceSlice writes
  to each node's driver; the controller writes no slices. Consider a
  follow-up VAP restricting who may create ModelClaims if clusters want
  policy there.

## Testing

- **Unit (Go):** resolve-JSON → template shape; drift repair; last-good
  retention on resolver failure; satisfiability math against fixture
  slices (incl. shortfall message).
- **Unit (Rust, upstream):** `--format json` golden output.
- **e2e Scenario 16 (CPU-only kind — the honest one):** apply a ModelClaim
  for a small catalog model → `Resolved=True`, template exists with the
  exact bounds from `status.resolved` → **`Satisfiable=False`** with a
  bandwidth shortfall, because `cpu0` publishes no `memoryBandwidthGBs`
  (verified in publisher.go). This asserts the whole loop *including* the
  refusal path, with no GPU.
- **e2e on GPU node (make scenarios, Strix Halo):** same ModelClaim with a
  model the 8060S fits → `Satisfiable=True` → pod referencing
  `resourceClaimTemplateName` reaches Running with `LLMFIT_*` env.
- **Negative:** unknown model → `Resolved=False (ModelNotFound)` + Event;
  template absent.

## Phasing and effort

| Phase | Content | Size |
|-------|---------|------|
| **M0** (llmfit) | `llmfit claim --format json` + golden test | ~½ day, releasable independently |
| **M1** (llmfit-dra) | CRD + controller (resolve → template, conditions, events), chart install of CRD/Deployment/RBAC, unit tests, Scenario 16, docs + examples/06 | ~3-4 days |
| **M2** | Satisfiability condition + candidates + shortfall reporting (slice informer) | ~1 day, separable |
| **M3 (later)** | serve-API resolution transport; curated flagship classes if quota demand appears; multi-model claims; Sympozium Model controller consuming ModelClaim | on demand |

## Risks

- **Upstream DRA evolution:** if SIG-Node ships parameterized DeviceClasses
  or claim-time class parameters, part of this becomes native. Mitigation:
  the resolver and the CRD's UX (status/satisfiability) survive either
  way; track the KEPs. v1alpha1 signals the API may move.
- **CRD lifecycle cost:** versioning/conversion if the schema evolves.
  Mitigation: keep v1alpha1 minimal (the spec above is 6 fields), no
  conversion webhooks until v1beta1 is earned.
- **Two sources of truth** (ModelClaim vs hand-written claims): fine —
  hand-written claims remain fully supported; ModelClaim is sugar with
  status, not a gate.

## Open questions

1. `minTps` default (20, matching the CLI) vs required-explicit? Default
   keeps the two-line quickstart; explicit avoids surprise bandwidth
   floors. *Leaning: default 20, documented loudly.*
2. Memory-only mode (`minTps` omitted ⇒ no bandwidth clause — "fits,
   speed be damned")? The resolver currently rejects `min_tps <= 0`;
   supporting it is a small upstream change. *Leaning: defer to demand.*
3. Should `status.resolved` also surface estimated tok/s on the *best
   current candidate* (marrying satisfiability with the estimator)?
   Attractive but couples status to the calibration loop — defer.
4. Cluster-scoped `ClusterModelClaim` for platform teams? Defer until a
   real multi-namespace consumer appears.
