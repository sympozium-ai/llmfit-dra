package modelclaim

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"sync"
	"time"

	resourceapi "k8s.io/api/resource/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"

	apiv1alpha1 "github.com/sympozium-ai/llmfit-dra/api/v1alpha1"
	"github.com/sympozium-ai/llmfit-dra/internal/observe"
)

// GVR of the ModelClaim custom resource.
var GVR = schema.GroupVersionResource{Group: apiv1alpha1.GroupName, Version: apiv1alpha1.GroupVersion.Version, Resource: "modelclaims"}

const (
	condResolved    = apiv1alpha1.ConditionResolved
	condSatisfiable = apiv1alpha1.ConditionSatisfiable
)

// fromUnstructured decodes a dynamic-client/informer object into the typed
// API. The CRD schema has already validated the document, so a failure here
// is a driver bug, not user input — callers surface it and requeue.
func fromUnstructured(u *unstructured.Unstructured) (*apiv1alpha1.ModelClaim, error) {
	mc := &apiv1alpha1.ModelClaim{}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, mc); err != nil {
		return nil, fmt.Errorf("decoding ModelClaim: %w", err)
	}
	return mc, nil
}

// modelRefPattern admits catalog names and HuggingFace-style org/name repo
// ids. First character alphanumeric, so a value can never read as a CLI flag
// to the exec resolver.
var modelRefPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*(/[A-Za-z0-9][A-Za-z0-9._-]*)?$`)

// Controller reconciles ModelClaims into same-named ResourceClaimTemplates.
// Availability model: if this controller is down, existing templates keep
// stamping per-pod claims — nothing here is in the scheduling or admission
// hot path.
type Controller struct {
	dyn      dynamic.Interface
	client   kubernetes.Interface
	resolver Resolver
	queue    workqueue.TypedRateLimitingInterface[string]

	mcInformer    cache.SharedIndexInformer
	sliceInformer cache.SharedIndexInformer
	claimInformer cache.SharedIndexInformer

	// Health is optional liveness/readiness plumbing: Ready fires after
	// cache sync, Beat on every processed item plus an idle tick.
	Health *observe.Health

	// resolveCache memoizes bounds by spec tuple: slice/claim churn re-runs
	// reconcile for every ModelClaim, and without the cache each run forks
	// the llmfit binary to recompute numbers that only change with the spec
	// (or a DB update — hence the TTL, matching the informer resync).
	resolveMu    sync.Mutex
	resolveCache map[string]resolveEntry

	// lastEvent suppresses consecutive duplicate events per object+reason —
	// a persistently failing resolve must not mint a fresh Event object on
	// every rate-limited retry.
	eventMu   sync.Mutex
	lastEvent map[string]string
}

type resolveEntry struct {
	bounds  *Bounds
	expires time.Time
}

const resolveCacheTTL = 10 * time.Minute

func New(dyn dynamic.Interface, client kubernetes.Interface, resolver Resolver) *Controller {
	c := &Controller{
		dyn:      dyn,
		client:   client,
		resolver: resolver,
		queue: workqueue.NewTypedRateLimitingQueue(
			workqueue.DefaultTypedControllerRateLimiter[string]()),
		resolveCache: map[string]resolveEntry{},
		lastEvent:    map[string]string{},
	}

	dynFactory := dynamicinformer.NewDynamicSharedInformerFactory(dyn, 10*time.Minute)
	c.mcInformer = dynFactory.ForResource(GVR).Informer()
	c.mcInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    c.enqueue,
		UpdateFunc: func(_, obj any) { c.enqueue(obj) },
		DeleteFunc: func(any) {}, // template GC'd via ownerRef
	})

	// ResourceSlice changes refresh Satisfiable for every claim (cheap: the
	// resync is a static evaluation over cached slices). Only OUR slices —
	// every other driver's churn is noise here.
	factory := informers.NewSharedInformerFactory(client, 10*time.Minute)
	c.sliceInformer = factory.Resource().V1().ResourceSlices().Informer()
	c.sliceInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj any) { c.enqueueAllIf(ownSlice(obj)) },
		UpdateFunc: func(old, obj any) { c.enqueueAllIf(ownSlice(old) || ownSlice(obj)) },
		DeleteFunc: func(obj any) { c.enqueueAllIf(ownSlice(obj)) },
	})

	// ResourceClaim allocations move devices between held and available, so
	// they refresh Satisfiable's availability numbers too (issue #21). Only
	// claims that hold (or held) one of our devices matter — per-pod claim
	// churn from other drivers must not fan out to every ModelClaim.
	c.claimInformer = factory.Resource().V1().ResourceClaims().Informer()
	c.claimInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj any) { c.enqueueAllIf(holdsOurDevice(obj)) },
		UpdateFunc: func(old, obj any) { c.enqueueAllIf(holdsOurDevice(old) || holdsOurDevice(obj)) },
		DeleteFunc: func(obj any) { c.enqueueAllIf(holdsOurDevice(obj)) },
	})

	return c
}

// ownSlice reports whether the informer object is one of this driver's
// ResourceSlices (tombstone-safe).
func ownSlice(obj any) bool {
	if t, ok := obj.(cache.DeletedFinalStateUnknown); ok {
		obj = t.Obj
	}
	s, ok := obj.(*resourceapi.ResourceSlice)
	return ok && s.Spec.Driver == DriverDomain
}

// holdsOurDevice reports whether the claim's allocation includes one of this
// driver's devices (tombstone-safe). Unallocated claims are irrelevant to
// Satisfiable's availability numbers.
func holdsOurDevice(obj any) bool {
	if t, ok := obj.(cache.DeletedFinalStateUnknown); ok {
		obj = t.Obj
	}
	rc, ok := obj.(*resourceapi.ResourceClaim)
	if !ok || rc.Status.Allocation == nil {
		return false
	}
	for _, r := range rc.Status.Allocation.Devices.Results {
		if r.Driver == DriverDomain {
			return true
		}
	}
	return false
}

func (c *Controller) enqueue(obj any) {
	if key, err := cache.MetaNamespaceKeyFunc(obj); err == nil {
		c.queue.Add(key)
	}
}

func (c *Controller) enqueueAll() {
	for _, obj := range c.mcInformer.GetStore().List() {
		c.enqueue(obj)
	}
}

func (c *Controller) enqueueAllIf(relevant bool) {
	if relevant {
		c.enqueueAll()
	}
}

// Run starts informers and the reconcile loop; blocks until ctx is done,
// then drains workers so shutdown cannot interrupt a template
// delete-then-recreate mid-flight.
func (c *Controller) Run(ctx context.Context, workers int) error {
	go c.mcInformer.Run(ctx.Done())
	go c.sliceInformer.Run(ctx.Done())
	go c.claimInformer.Run(ctx.Done())
	if !cache.WaitForCacheSync(ctx.Done(), c.mcInformer.HasSynced, c.sliceInformer.HasSynced, c.claimInformer.HasSynced) {
		return fmt.Errorf("informer caches did not sync")
	}
	klog.FromContext(ctx).Info("modelclaim controller started")
	if c.Health != nil {
		c.Health.Ready()
		c.Health.Beat()
		// Idle heartbeat: liveness must not fail on a quiet cluster where no
		// item arrives within the staleness window. Worker beats (processNext)
		// cover the busy case.
		go func() {
			t := time.NewTicker(30 * time.Second)
			defer t.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-t.C:
					c.Health.Beat()
				}
			}
		}()
	}

	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for c.processNext(ctx) {
			}
		}()
	}
	<-ctx.Done()
	c.queue.ShutDown()
	wg.Wait()
	return nil
}

func (c *Controller) processNext(ctx context.Context) bool {
	key, shutdown := c.queue.Get()
	if shutdown {
		return false
	}
	defer c.queue.Done(key)

	// Detached from shutdown cancellation (but bounded): a SIGTERM must not
	// cancel a reconcile between a template delete and its recreate.
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
	defer cancel()
	if err := c.reconcile(rctx, key); err != nil {
		observe.ModelClaimReconcile("error")
		klog.FromContext(ctx).Error(err, "reconcile failed", "modelclaim", key)
		c.queue.AddRateLimited(key)
	} else {
		observe.ModelClaimReconcile("ok")
		c.queue.Forget(key)
	}
	if c.Health != nil {
		c.Health.Beat()
	}
	return true
}

func (c *Controller) reconcile(ctx context.Context, key string) error {
	ns, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		return nil
	}
	obj, exists, err := c.mcInformer.GetStore().GetByKey(key)
	if err != nil || !exists {
		return err // deleted: ownerRef GC removes the template
	}
	mc, err := fromUnstructured(obj.(*unstructured.Unstructured))
	if err != nil {
		return err
	}

	model := mc.Spec.Model
	// The API server defaults minTps/efficiencyPct/deviceClassName from the
	// CRD schema; the fallbacks below only cover objects persisted before the
	// defaults existed.
	minTps := 20.0
	if mc.Spec.MinTps != nil {
		minTps = *mc.Spec.MinTps
	}
	quant := mc.Spec.Quant
	var efficiency int64
	if mc.Spec.EfficiencyPct != nil {
		efficiency = int64(*mc.Spec.EfficiencyPct)
	}
	deviceClass := mc.Spec.DeviceClassName
	if deviceClass == "" {
		deviceClass = DriverDomain
	}
	extraSelectors := mc.Spec.ExtraSelectors

	// ── Validate ───────────────────────────────────────────────────────
	// The model reaches the llmfit binary as an argv token: reject anything
	// that isn't a plain catalog/HF reference so a leading-dash value can
	// never be parsed as a flag. Terminal until the spec changes — no requeue.
	if !modelRefPattern.MatchString(model) {
		msg := fmt.Sprintf("spec.model %q is not a valid model reference (want org/name or name, e.g. Qwen/Qwen3.6-30B-A3B)", model)
		c.event(mc, "Warning", apiv1alpha1.ReasonInvalidModel, msg)
		return c.updateStatus(ctx, ns, name, func(status *apiv1alpha1.ModelClaimStatus) {
			setCondition(status, mc.Generation, condResolved, metav1.ConditionFalse, apiv1alpha1.ReasonInvalidModel, msg)
			setCondition(status, mc.Generation, condSatisfiable, metav1.ConditionUnknown, apiv1alpha1.ReasonInvalidModel, "model not resolved")
		})
	}

	// ── Resolve ────────────────────────────────────────────────────────
	bounds, resolveErr := c.resolve(ctx, model, minTps, quant, efficiency)
	if resolveErr != nil {
		// Never touch an existing template on resolve failure — a model-DB
		// hiccup must not cascade into scheduling failures for new pods.
		// Satisfiable goes Unknown in the same write: leaving the previous
		// verdict (and printed columns) standing would contradict a False
		// Resolved. The returned error requeues with backoff, so a transient
		// failure on a NEW ModelClaim doesn't strand it template-less until
		// the next resync.
		c.event(mc, "Warning", apiv1alpha1.ReasonResolveFailed, resolveErr.Error())
		if err := c.updateStatus(ctx, ns, name, func(status *apiv1alpha1.ModelClaimStatus) {
			setCondition(status, mc.Generation, condResolved, metav1.ConditionFalse, apiv1alpha1.ReasonResolveFailed, resolveErr.Error())
			setCondition(status, mc.Generation, condSatisfiable, metav1.ConditionUnknown, apiv1alpha1.ReasonResolveFailed, "bounds unavailable while resolve fails")
		}); err != nil {
			return err
		}
		return fmt.Errorf("resolving model %q: %w", model, resolveErr)
	}

	// ── Reconcile the same-named ResourceClaimTemplate ─────────────────
	desired := BuildTemplate(mc, bounds, deviceClass, extraSelectors)
	templates := c.client.ResourceV1().ResourceClaimTemplates(ns)
	live, err := templates.Get(ctx, name, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		if _, err := templates.Create(ctx, desired, metav1.CreateOptions{}); err != nil {
			return fmt.Errorf("creating template: %w", err)
		}
		c.event(mc, "Normal", "TemplateCreated",
			fmt.Sprintf("ResourceClaimTemplate %s: memory>=%dGi, bandwidth>=%dGB/s (%s)",
				name, bounds.MemoryGi, bounds.MinBandwidthGBs, bounds.Quant))
	case err != nil:
		return fmt.Errorf("getting template: %w", err)
	case !metav1.IsControlledBy(live, mc):
		// A same-named template we do NOT own: never adopt, never update,
		// and above all never delete-recreate it. Terminal until the user
		// renames one of the two — say so instead of clobbering.
		msg := fmt.Sprintf("a ResourceClaimTemplate named %q already exists and is not managed by this ModelClaim; rename the ModelClaim or delete the template", name)
		c.event(mc, "Warning", apiv1alpha1.ReasonTemplateConflict, msg)
		return c.updateStatus(ctx, ns, name, func(status *apiv1alpha1.ModelClaimStatus) {
			setCondition(status, mc.Generation, condResolved, metav1.ConditionFalse, apiv1alpha1.ReasonTemplateConflict, msg)
			setCondition(status, mc.Generation, condSatisfiable, metav1.ConditionUnknown, apiv1alpha1.ReasonTemplateConflict, "template not managed by this ModelClaim")
		})
	case TemplateNeedsUpdate(live, desired):
		updated := live.DeepCopy()
		updated.Spec = desired.Spec
		if updated.Annotations == nil {
			updated.Annotations = map[string]string{}
		}
		for k, v := range desired.Annotations {
			updated.Annotations[k] = v
		}
		if _, err := templates.Update(ctx, updated, metav1.UpdateOptions{}); err != nil {
			if apierrors.IsInvalid(err) {
				// Template spec immutable on this API server: recreate under
				// the same name. Already-stamped per-pod claims are unaffected.
				if delErr := templates.Delete(ctx, name, metav1.DeleteOptions{}); delErr != nil {
					return fmt.Errorf("deleting immutable template for recreate: %w", delErr)
				}
				if _, createErr := templates.Create(ctx, desired, metav1.CreateOptions{}); createErr != nil {
					return fmt.Errorf("recreating template: %w", createErr)
				}
			} else {
				return fmt.Errorf("updating template: %w", err)
			}
		}
		c.event(mc, "Normal", "TemplateUpdated",
			fmt.Sprintf("bounds now memory>=%dGi, bandwidth>=%dGB/s (resolver %s)",
				bounds.MemoryGi, bounds.MinBandwidthGBs, bounds.ResolverVersion))
	}

	// ── Satisfiability (advisory) ──────────────────────────────────────
	cands := c.evaluateCandidates(bounds, deviceClass, computeFloor(mc))
	satStatus, satReason, satMsg := satisfiableCondition(cands)

	// Events on TRANSITIONS only: `kubectl get events -w` during a rollout
	// should show flips (fits→doesn't fit, restored), not one line per
	// reconcile.
	if prev := conditionReason(mc, condSatisfiable); prev != satReason {
		kind := "Normal"
		if satStatus == metav1.ConditionFalse {
			kind = "Warning"
		}
		c.event(mc, kind, satReason, satMsg)
	}

	return c.updateStatus(ctx, ns, name, func(status *apiv1alpha1.ModelClaimStatus) {
		status.ObservedGeneration = mc.Generation
		status.Resolved = &apiv1alpha1.ResolvedPhysics{
			MemoryGi:        int64(bounds.MemoryGi),
			MinBandwidthGBs: int64(bounds.MinBandwidthGBs),
			Quant:           bounds.Quant,
			WeightsGb:       strconv.FormatFloat(bounds.WeightsGb, 'f', 1, 64),
			ResolverVersion: bounds.ResolverVersion,
		}
		status.TemplateRef = &apiv1alpha1.TemplateRef{Name: name}
		status.Candidates = &apiv1alpha1.Candidates{
			Devices:   int64(cands.Devices),
			Nodes:     int64(cands.Nodes),
			Available: int64(cands.Available),
		}
		setCondition(status, mc.Generation, condResolved, metav1.ConditionTrue, apiv1alpha1.ReasonResolved,
			fmt.Sprintf("%s @ %s: memory>=%dGi, bandwidth>=%dGB/s",
				bounds.Model, bounds.Quant, bounds.MemoryGi, bounds.MinBandwidthGBs))
		setCondition(status, mc.Generation, condSatisfiable, satStatus, satReason, satMsg)
	})
}

// satisfiableCondition maps a candidates snapshot to the Satisfiable
// condition tuple.
func satisfiableCondition(cands Candidates) (status metav1.ConditionStatus, reason, message string) {
	switch {
	case cands.Devices > 0 && cands.Available > 0:
		return metav1.ConditionTrue, apiv1alpha1.ReasonDevicesAvailable,
			fmt.Sprintf("%d device(s) on %d node(s) satisfy the bounds (%d currently unallocated)",
				cands.Devices, cands.Nodes, cands.Available)
	case cands.Devices > 0:
		// Physics fits but every matching device is held by another claim:
		// satisfiable in principle, queued in practice — say so (issue #21;
		// this exact ambiguity misled a live operator).
		return metav1.ConditionTrue, apiv1alpha1.ReasonAllDevicesHeld,
			fmt.Sprintf("%d device(s) satisfy the bounds but 0 are unallocated (e.g. held by %s) — pods will queue until a claim releases",
				cands.Devices, cands.HeldBy)
	default:
		return metav1.ConditionFalse, apiv1alpha1.ReasonNoCandidates, cands.Shortfall
	}
}

// conditionReason reads the reason of a condition from the (informer-cached)
// object's status; "" when absent.
func conditionReason(mc *apiv1alpha1.ModelClaim, condType string) string {
	if cond := apimeta.FindStatusCondition(mc.Status.Conditions, condType); cond != nil {
		return cond.Reason
	}
	return ""
}

// resolve memoizes Resolver.Resolve by spec tuple with a TTL. Errors are not
// cached: retry pacing is the rate limiter's job, and a fixed model DB must
// take effect on the next attempt.
func (c *Controller) resolve(ctx context.Context, model string, minTps float64, quant string, efficiency int64) (*Bounds, error) {
	key := fmt.Sprintf("%s|%s|%s|%d",
		model, strconv.FormatFloat(minTps, 'f', -1, 64), quant, efficiency)
	now := time.Now()

	c.resolveMu.Lock()
	if e, ok := c.resolveCache[key]; ok && now.Before(e.expires) {
		c.resolveMu.Unlock()
		observe.ObserveResolve(0, "cached")
		return e.bounds, nil
	}
	c.resolveMu.Unlock()

	start := now
	bounds, err := c.resolver.Resolve(ctx, model, minTps, quant, efficiency)
	if err != nil {
		observe.ObserveResolve(time.Since(start), "error")
		return nil, err
	}
	observe.ObserveResolve(time.Since(start), "ok")

	c.resolveMu.Lock()
	if len(c.resolveCache) >= 512 {
		// Cheap bound: churn past the cap is pathological (spec tuples are
		// few); dropping everything is simpler than an LRU and self-heals.
		c.resolveCache = map[string]resolveEntry{}
	}
	c.resolveCache[key] = resolveEntry{bounds: bounds, expires: now.Add(resolveCacheTTL)}
	c.resolveMu.Unlock()
	return bounds, nil
}

func (c *Controller) evaluateCandidates(b *Bounds, deviceClass string, minComputeTFLOPS int64) Candidates {
	var slices []*resourceapi.ResourceSlice
	for _, obj := range c.sliceInformer.GetStore().List() {
		if s, ok := obj.(*resourceapi.ResourceSlice); ok {
			slices = append(slices, s)
		}
	}
	var claims []*resourceapi.ResourceClaim
	for _, obj := range c.claimInformer.GetStore().List() {
		if rc, ok := obj.(*resourceapi.ResourceClaim); ok {
			claims = append(claims, rc)
		}
	}
	return EvaluateSlices(slices, AllocatedDevices(claims), b, deviceClass, minComputeTFLOPS)
}

// updateStatus mutates .status via the status subresource with a fresh GET
// (informer cache may be stale right after our own writes).
func (c *Controller) updateStatus(ctx context.Context, ns, name string, mutate func(*apiv1alpha1.ModelClaimStatus)) error {
	fresh, err := c.dyn.Resource(GVR).Namespace(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return err
	}
	mc, err := fromUnstructured(fresh)
	if err != nil {
		return err
	}
	mutate(&mc.Status)
	obj, err := runtime.DefaultUnstructuredConverter.ToUnstructured(mc)
	if err != nil {
		return fmt.Errorf("encoding ModelClaim: %w", err)
	}
	_, err = c.dyn.Resource(GVR).Namespace(ns).UpdateStatus(ctx, &unstructured.Unstructured{Object: obj}, metav1.UpdateOptions{})
	if apierrors.IsConflict(err) {
		return nil // requeued by the informer's next event
	}
	return err
}

func (c *Controller) event(mc *apiv1alpha1.ModelClaim, kind, reason, message string) {
	// Suppress consecutive duplicates per object+reason: a persistently
	// failing resolve retries on a rate-limited backoff, and each retry
	// would otherwise mint a fresh Event object.
	dedupKey := mc.GetNamespace() + "/" + mc.GetName() + "/" + reason
	c.eventMu.Lock()
	if c.lastEvent[dedupKey] == message {
		c.eventMu.Unlock()
		return
	}
	c.lastEvent[dedupKey] = message
	if len(c.lastEvent) > 4096 { // bound memory on pathological churn
		c.lastEvent = map[string]string{dedupKey: message}
	}
	c.eventMu.Unlock()

	// Best-effort Event via the core API (no broadcaster machinery needed
	// for the handful of events this controller emits).
	now := metav1.Now()
	ev := eventFor(mc, kind, reason, message, now)
	_, err := c.client.CoreV1().Events(mc.GetNamespace()).CreateWithEventNamespace(&ev)
	if err != nil {
		klog.Background().V(4).Info("event emit failed", "err", err)
	}
}

// setCondition upserts a condition, preserving lastTransitionTime when
// status is unchanged (apimeta.SetStatusCondition semantics).
func setCondition(status *apiv1alpha1.ModelClaimStatus, generation int64, condType string, condStatus metav1.ConditionStatus, reason, message string) {
	apimeta.SetStatusCondition(&status.Conditions, metav1.Condition{
		Type:               condType,
		Status:             condStatus,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: generation,
	})
}
