package modelclaim

import (
	"context"
	"fmt"
	"strconv"
	"time"

	resourceapi "k8s.io/api/resource/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"
)

// GVR of the ModelClaim custom resource.
var GVR = schema.GroupVersionResource{Group: "llmfit.ai", Version: "v1alpha1", Resource: "modelclaims"}

const (
	condResolved    = "Resolved"
	condSatisfiable = "Satisfiable"
)

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
}

func New(dyn dynamic.Interface, client kubernetes.Interface, resolver Resolver) *Controller {
	c := &Controller{
		dyn:      dyn,
		client:   client,
		resolver: resolver,
		queue: workqueue.NewTypedRateLimitingQueue(
			workqueue.DefaultTypedControllerRateLimiter[string]()),
	}

	dynFactory := dynamicinformer.NewDynamicSharedInformerFactory(dyn, 10*time.Minute)
	c.mcInformer = dynFactory.ForResource(GVR).Informer()
	c.mcInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    c.enqueue,
		UpdateFunc: func(_, obj any) { c.enqueue(obj) },
		DeleteFunc: func(any) {}, // template GC'd via ownerRef
	})

	// ResourceSlice changes refresh Satisfiable for every claim (cheap: the
	// resync is a static evaluation over cached slices).
	factory := informers.NewSharedInformerFactory(client, 10*time.Minute)
	c.sliceInformer = factory.Resource().V1().ResourceSlices().Informer()
	c.sliceInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(any) { c.enqueueAll() },
		UpdateFunc: func(_, obj any) { c.enqueueAll() },
		DeleteFunc: func(any) { c.enqueueAll() },
	})

	// ResourceClaim allocations move devices between held and available, so
	// they refresh Satisfiable's availability numbers too (issue #21).
	c.claimInformer = factory.Resource().V1().ResourceClaims().Informer()
	c.claimInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(any) { c.enqueueAll() },
		UpdateFunc: func(_, obj any) { c.enqueueAll() },
		DeleteFunc: func(any) { c.enqueueAll() },
	})

	return c
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

// Run starts informers and the reconcile loop; blocks until ctx is done.
func (c *Controller) Run(ctx context.Context, workers int) error {
	defer c.queue.ShutDown()

	go c.mcInformer.Run(ctx.Done())
	go c.sliceInformer.Run(ctx.Done())
	go c.claimInformer.Run(ctx.Done())
	if !cache.WaitForCacheSync(ctx.Done(), c.mcInformer.HasSynced, c.sliceInformer.HasSynced, c.claimInformer.HasSynced) {
		return fmt.Errorf("informer caches did not sync")
	}
	klog.FromContext(ctx).Info("modelclaim controller started")

	for i := 0; i < workers; i++ {
		go func() {
			for c.processNext(ctx) {
			}
		}()
	}
	<-ctx.Done()
	return nil
}

func (c *Controller) processNext(ctx context.Context) bool {
	key, shutdown := c.queue.Get()
	if shutdown {
		return false
	}
	defer c.queue.Done(key)

	if err := c.reconcile(ctx, key); err != nil {
		klog.FromContext(ctx).Error(err, "reconcile failed", "modelclaim", key)
		c.queue.AddRateLimited(key)
	} else {
		c.queue.Forget(key)
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
	mc := obj.(*unstructured.Unstructured).DeepCopy()

	spec, _, _ := unstructured.NestedMap(mc.Object, "spec")
	model, _ := spec["model"].(string)
	minTps := numOr(spec["minTps"], 20)
	quant, _ := spec["quant"].(string)
	efficiency := int64(numOr(spec["efficiencyPct"], 0))
	deviceClass, _ := spec["deviceClassName"].(string)
	if deviceClass == "" {
		deviceClass = DriverDomain
	}
	extraSelectors := strSlice(spec["extraSelectors"])

	// ── Resolve ────────────────────────────────────────────────────────
	bounds, resolveErr := c.resolver.Resolve(ctx, model, minTps, quant, efficiency)
	if resolveErr != nil {
		// Never touch an existing template on resolve failure — a model-DB
		// hiccup must not cascade into scheduling failures for new pods.
		c.event(mc, "Warning", "ResolveFailed", resolveErr.Error())
		return c.updateStatus(ctx, ns, name, func(status map[string]any) {
			setCondition(status, mc.GetGeneration(), condResolved, "False", "ResolveFailed", resolveErr.Error())
		})
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
	cands := c.evaluateCandidates(bounds, deviceClass)

	return c.updateStatus(ctx, ns, name, func(status map[string]any) {
		status["observedGeneration"] = mc.GetGeneration()
		status["resolved"] = map[string]any{
			"memoryGi":        int64(bounds.MemoryGi),
			"minBandwidthGBs": int64(bounds.MinBandwidthGBs),
			"quant":           bounds.Quant,
			"weightsGb":       strconv.FormatFloat(bounds.WeightsGb, 'f', 1, 64),
			"resolverVersion": bounds.ResolverVersion,
		}
		status["templateRef"] = map[string]any{"name": name}
		status["candidates"] = map[string]any{
			"devices":   int64(cands.Devices),
			"nodes":     int64(cands.Nodes),
			"available": int64(cands.Available),
		}
		setCondition(status, mc.GetGeneration(), condResolved, "True", "Resolved",
			fmt.Sprintf("%s @ %s: memory>=%dGi, bandwidth>=%dGB/s",
				bounds.Model, bounds.Quant, bounds.MemoryGi, bounds.MinBandwidthGBs))
		switch {
		case cands.Devices > 0 && cands.Available > 0:
			setCondition(status, mc.GetGeneration(), condSatisfiable, "True", "DevicesAvailable",
				fmt.Sprintf("%d device(s) on %d node(s) satisfy the bounds (%d currently unallocated)",
					cands.Devices, cands.Nodes, cands.Available))
		case cands.Devices > 0:
			// Physics fits but every matching device is held by another
			// claim: satisfiable in principle, queued in practice — say so
			// (issue #21; this exact ambiguity misled a live operator).
			setCondition(status, mc.GetGeneration(), condSatisfiable, "True", "AllDevicesHeld",
				fmt.Sprintf("%d device(s) satisfy the bounds but 0 are unallocated (e.g. held by %s) — pods will queue until a claim releases",
					cands.Devices, cands.HeldBy))
		default:
			setCondition(status, mc.GetGeneration(), condSatisfiable, "False", "NoCandidates", cands.Shortfall)
		}
	})
}

func (c *Controller) evaluateCandidates(b *Bounds, deviceClass string) Candidates {
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
	return EvaluateSlices(slices, AllocatedDevices(claims), b, deviceClass)
}

// updateStatus mutates .status via the status subresource with a fresh GET
// (informer cache may be stale right after our own writes).
func (c *Controller) updateStatus(ctx context.Context, ns, name string, mutate func(map[string]any)) error {
	fresh, err := c.dyn.Resource(GVR).Namespace(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return err
	}
	status, _, _ := unstructured.NestedMap(fresh.Object, "status")
	if status == nil {
		status = map[string]any{}
	}
	mutate(status)
	_ = unstructured.SetNestedMap(fresh.Object, status, "status")
	_, err = c.dyn.Resource(GVR).Namespace(ns).UpdateStatus(ctx, fresh, metav1.UpdateOptions{})
	if apierrors.IsConflict(err) {
		return nil // requeued by the informer's next event
	}
	return err
}

func (c *Controller) event(mc *unstructured.Unstructured, kind, reason, message string) {
	// Best-effort Event via the core API (no broadcaster machinery needed
	// for the handful of events this controller emits).
	now := metav1.Now()
	ev := eventFor(mc, kind, reason, message, now)
	_, err := c.client.CoreV1().Events(mc.GetNamespace()).CreateWithEventNamespace(&ev)
	if err != nil {
		klog.Background().V(4).Info("event emit failed", "err", err)
	}
}

// setCondition upserts a metav1.Condition-shaped entry, preserving
// lastTransitionTime when status is unchanged.
func setCondition(status map[string]any, generation int64, condType, condStatus, reason, message string) {
	conds, _ := status["conditions"].([]any)
	now := time.Now().UTC().Format(time.RFC3339)
	newCond := map[string]any{
		"type":               condType,
		"status":             condStatus,
		"reason":             reason,
		"message":            message,
		"observedGeneration": generation,
		"lastTransitionTime": now,
	}
	for i, c := range conds {
		if m, ok := c.(map[string]any); ok && m["type"] == condType {
			if m["status"] == condStatus {
				newCond["lastTransitionTime"] = m["lastTransitionTime"]
			}
			conds[i] = newCond
			status["conditions"] = conds
			return
		}
	}
	status["conditions"] = append(conds, any(newCond))
}

func numOr(v any, def float64) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case int64:
		return float64(n)
	default:
		return def
	}
}

func strSlice(v any) []string {
	items, _ := v.([]any)
	out := make([]string, 0, len(items))
	for _, it := range items {
		if s, ok := it.(string); ok {
			out = append(out, s)
		}
	}
	return out
}
