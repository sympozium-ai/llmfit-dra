package modelclaim

import (
	"context"
	"errors"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	resourceapi "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynfake "k8s.io/client-go/dynamic/fake"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/utils/ptr"

	apiv1alpha1 "github.com/sympozium-ai/llmfit-dra/api/v1alpha1"
	"github.com/sympozium-ai/llmfit-dra/internal/index"
)

// fakeResolver returns canned bounds (or a canned error) and counts calls, so
// tests can assert the memoization behavior of Controller.resolve.
type fakeResolver struct {
	bounds *Bounds
	err    error
	calls  int
}

func (f *fakeResolver) Resolve(_ context.Context, _ string, _ float64, _ string, _ int64) (*Bounds, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return f.bounds, nil
}

// reconcileMC builds a ModelClaim with enough metadata for the reconcile
// path: UID (ownerRef), generation (observedGeneration), and a spec.
func reconcileMC(model string) *apiv1alpha1.ModelClaim {
	mc := testMC()
	mc.Generation = 3
	mc.Spec.Model = model
	mc.Spec.MinTps = ptr.To(20.0)
	return mc
}

// mustUnstructured converts a typed ModelClaim into the unstructured shape
// the dynamic informer/client delivers in production.
func mustUnstructured(t *testing.T, mc *apiv1alpha1.ModelClaim) *unstructured.Unstructured {
	t.Helper()
	obj, err := runtime.DefaultUnstructuredConverter.ToUnstructured(mc)
	if err != nil {
		t.Fatalf("to unstructured: %v", err)
	}
	return &unstructured.Unstructured{Object: obj}
}

// testBoards loads the embedded NVIDIA board table — real data, so tests
// exercise the same table production ships.
func testBoards(t *testing.T) *index.NvidiaBoards {
	t.Helper()
	boards, err := index.LoadNvidiaBoards()
	if err != nil {
		t.Fatalf("loading nvidia boards: %v", err)
	}
	return boards
}

// newTestController wires a Controller against fake clients and injects the
// ModelClaim into both the dynamic tracker (updateStatus does a fresh GET)
// and the informer store (reconcile reads from it) — as unstructured, the
// shape production informers deliver. Informers are never started —
// reconcile is driven directly.
func newTestController(t *testing.T, r Resolver, mc *apiv1alpha1.ModelClaim, k8sObjs ...runtime.Object) (*Controller, *dynfake.FakeDynamicClient, *k8sfake.Clientset) {
	t.Helper()
	u := mustUnstructured(t, mc)
	dyn := dynfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{GVR: "ModelClaimList"}, u)
	cs := k8sfake.NewClientset(k8sObjs...)
	c := New(dyn, cs, r, testBoards(t))
	if err := c.mcInformer.GetStore().Add(u); err != nil {
		t.Fatalf("seeding informer store: %v", err)
	}
	return c, dyn, cs
}

// condition reads (status, reason) of a condition from the ModelClaim as
// stored in the fake dynamic tracker.
func condition(t *testing.T, dyn *dynfake.FakeDynamicClient, ns, name, condType string) (status, reason string) {
	t.Helper()
	mc, err := dyn.Resource(GVR).Namespace(ns).Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("getting ModelClaim: %v", err)
	}
	conds, _, _ := unstructured.NestedSlice(mc.Object, "status", "conditions")
	for _, c := range conds {
		if m, ok := c.(map[string]any); ok && m["type"] == condType {
			s, _ := m["status"].(string)
			r, _ := m["reason"].(string)
			return s, r
		}
	}
	t.Fatalf("condition %s not found in %v", condType, conds)
	return "", ""
}

// eventReasons lists the reasons of all Events created in the fake clientset,
// in order. Inspecting Actions() keeps this independent of whether the fake
// tracker accepts generateName-only objects.
func eventReasons(cs *k8sfake.Clientset) []string {
	var out []string
	for _, a := range cs.Actions() {
		if a.GetVerb() != "create" || a.GetResource().Resource != "events" {
			continue
		}
		if ev, ok := a.(k8stesting.CreateAction).GetObject().(*corev1.Event); ok {
			out = append(out, ev.Reason)
		}
	}
	return out
}

func countReason(reasons []string, want string) int {
	n := 0
	for _, r := range reasons {
		if r == want {
			n++
		}
	}
	return n
}

func TestReconcileCreatesTemplate(t *testing.T) {
	mc := reconcileMC("Qwen/Qwen3-30B-A3B")
	c, dyn, cs := newTestController(t, &fakeResolver{bounds: testBounds()}, mc)

	if err := c.reconcile(context.Background(), "team-a/qwen3"); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	tpl, err := cs.ResourceV1().ResourceClaimTemplates("team-a").Get(context.Background(), "qwen3", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("template not created: %v", err)
	}
	if !metav1.IsControlledBy(tpl, mc) {
		t.Fatalf("template must be controller-owned by the ModelClaim: %+v", tpl.OwnerReferences)
	}
	if tpl.OwnerReferences[0].UID != mc.GetUID() {
		t.Errorf("ownerRef UID = %q, want %q", tpl.OwnerReferences[0].UID, mc.GetUID())
	}
	cel := tpl.Spec.Spec.Devices.Requests[0].Exactly.Selectors[0].CEL.Expression
	for _, want := range []string{"quantity('18Gi')", "memoryBandwidthGBs >= 160"} {
		if !strings.Contains(cel, want) {
			t.Errorf("fit CEL missing %q:\n%s", want, cel)
		}
	}

	if s, r := condition(t, dyn, "team-a", "qwen3", condResolved); s != "True" || r != "Resolved" {
		t.Errorf("Resolved = %s/%s, want True/Resolved", s, r)
	}
	fresh, err := dyn.Resource(GVR).Namespace("team-a").Get(context.Background(), "qwen3", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if gen, _, _ := unstructured.NestedInt64(fresh.Object, "status", "observedGeneration"); gen != 3 {
		t.Errorf("status.observedGeneration = %d, want 3", gen)
	}
}

func TestReconcileResolveFailureRequeuesAndSetsConditions(t *testing.T) {
	mc := reconcileMC("Qwen/Qwen3-30B-A3B")
	c, dyn, cs := newTestController(t, &fakeResolver{err: errors.New("model DB unavailable")}, mc)

	if err := c.reconcile(context.Background(), "team-a/qwen3"); err == nil {
		t.Fatal("want error for rate-limited requeue on resolve failure")
	}
	if _, err := cs.ResourceV1().ResourceClaimTemplates("team-a").Get(context.Background(), "qwen3", metav1.GetOptions{}); err == nil {
		t.Fatal("no template must be created on resolve failure")
	}
	if s, r := condition(t, dyn, "team-a", "qwen3", condResolved); s != "False" || r != "ResolveFailed" {
		t.Errorf("Resolved = %s/%s, want False/ResolveFailed", s, r)
	}
	if s, r := condition(t, dyn, "team-a", "qwen3", condSatisfiable); s != "Unknown" || r != "ResolveFailed" {
		t.Errorf("Satisfiable = %s/%s, want Unknown/ResolveFailed", s, r)
	}
}

func TestReconcileInvalidModelIsTerminal(t *testing.T) {
	mc := reconcileMC("--config=/etc/passwd")
	resolver := &fakeResolver{bounds: testBounds()}
	c, dyn, cs := newTestController(t, resolver, mc)

	if err := c.reconcile(context.Background(), "team-a/qwen3"); err != nil {
		t.Fatalf("invalid model is terminal, want nil (no requeue), got %v", err)
	}
	if resolver.calls != 0 {
		t.Errorf("resolver must not run for an invalid model, ran %d times", resolver.calls)
	}
	if _, err := cs.ResourceV1().ResourceClaimTemplates("team-a").Get(context.Background(), "qwen3", metav1.GetOptions{}); err == nil {
		t.Fatal("no template must be created for an invalid model")
	}
	if s, r := condition(t, dyn, "team-a", "qwen3", condResolved); s != "False" || r != "InvalidModel" {
		t.Errorf("Resolved = %s/%s, want False/InvalidModel", s, r)
	}
}

func TestReconcileRefusesForeignTemplate(t *testing.T) {
	mc := reconcileMC("Qwen/Qwen3-30B-A3B")
	foreign := &resourceapi.ResourceClaimTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: "qwen3", Namespace: "team-a"},
		Spec: resourceapi.ResourceClaimTemplateSpec{
			Spec: resourceapi.ResourceClaimSpec{
				Devices: resourceapi.DeviceClaim{
					Requests: []resourceapi.DeviceRequest{{Name: "someone-elses"}},
				},
			},
		},
	}
	c, dyn, cs := newTestController(t, &fakeResolver{bounds: testBounds()}, mc, foreign)

	if err := c.reconcile(context.Background(), "team-a/qwen3"); err != nil {
		t.Fatalf("conflict is terminal, want nil (no requeue), got %v", err)
	}

	live, err := cs.ResourceV1().ResourceClaimTemplates("team-a").Get(context.Background(), "qwen3", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("foreign template must not be deleted: %v", err)
	}
	if len(live.OwnerReferences) != 0 {
		t.Errorf("foreign template must not be adopted: %+v", live.OwnerReferences)
	}
	if len(live.Spec.Spec.Devices.Requests) != 1 || live.Spec.Spec.Devices.Requests[0].Name != "someone-elses" {
		t.Errorf("foreign template spec was modified: %+v", live.Spec.Spec.Devices.Requests)
	}
	if s, r := condition(t, dyn, "team-a", "qwen3", condResolved); s != "False" || r != "TemplateConflict" {
		t.Errorf("Resolved = %s/%s, want False/TemplateConflict", s, r)
	}
	if countReason(eventReasons(cs), "TemplateConflict") != 1 {
		t.Errorf("want one TemplateConflict event, got %v", eventReasons(cs))
	}
}

func TestReconcileUpdatesOwnedTemplate(t *testing.T) {
	mc := reconcileMC("Qwen/Qwen3-30B-A3B")
	stale := testBounds()
	stale.MinBandwidthGBs = 100
	stale.MemoryGi = 12
	owned := BuildTemplate(mc, stale, DriverDomain, nil, nil)
	c, _, cs := newTestController(t, &fakeResolver{bounds: testBounds()}, mc, owned)

	if err := c.reconcile(context.Background(), "team-a/qwen3"); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	live, err := cs.ResourceV1().ResourceClaimTemplates("team-a").Get(context.Background(), "qwen3", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	cel := live.Spec.Spec.Devices.Requests[0].Exactly.Selectors[0].CEL.Expression
	for _, want := range []string{"quantity('18Gi')", "memoryBandwidthGBs >= 160"} {
		if !strings.Contains(cel, want) {
			t.Errorf("template not updated to new bounds, CEL missing %q:\n%s", want, cel)
		}
	}
	if !metav1.IsControlledBy(live, mc) {
		t.Errorf("update must preserve ownership: %+v", live.OwnerReferences)
	}
	if countReason(eventReasons(cs), "TemplateUpdated") != 1 {
		t.Errorf("want one TemplateUpdated event, got %v", eventReasons(cs))
	}
}

func TestResolveMemoized(t *testing.T) {
	mc := reconcileMC("Qwen/Qwen3-30B-A3B")
	resolver := &fakeResolver{bounds: testBounds()}
	c, _, _ := newTestController(t, resolver, mc)

	for i := 0; i < 2; i++ {
		if err := c.reconcile(context.Background(), "team-a/qwen3"); err != nil {
			t.Fatalf("reconcile %d: %v", i, err)
		}
	}
	if resolver.calls != 1 {
		t.Errorf("resolver called %d times for an unchanged spec, want 1 (cache hit)", resolver.calls)
	}
}

func TestSatisfiableTransitionEmitsEventOnce(t *testing.T) {
	mc := reconcileMC("Qwen/Qwen3-30B-A3B")
	c, dyn, cs := newTestController(t, &fakeResolver{bounds: testBounds()}, mc)
	// One published device that misses the bandwidth floor (100 < 160).
	if err := c.sliceInformer.GetStore().Add(slice("strix", device("gpu0", "gpu", 96, 100, true, false))); err != nil {
		t.Fatal(err)
	}

	if err := c.reconcile(context.Background(), "team-a/qwen3"); err != nil {
		t.Fatalf("first reconcile: %v", err)
	}
	if got := countReason(eventReasons(cs), "NoCandidates"); got != 1 {
		t.Fatalf("want one NoCandidates event after first reconcile, got %d (%v)", got, eventReasons(cs))
	}

	// Simulate the informer catching up with our own status write: reconcile
	// reads the previous Satisfiable reason from the informer copy.
	fresh, err := dyn.Resource(GVR).Namespace("team-a").Get(context.Background(), "qwen3", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.mcInformer.GetStore().Update(fresh); err != nil {
		t.Fatal(err)
	}

	if err := c.reconcile(context.Background(), "team-a/qwen3"); err != nil {
		t.Fatalf("second reconcile: %v", err)
	}
	if got := countReason(eventReasons(cs), "NoCandidates"); got != 1 {
		t.Errorf("unchanged state must not re-emit NoCandidates, got %d events (%v)", got, eventReasons(cs))
	}
	if s, r := condition(t, dyn, "team-a", "qwen3", condSatisfiable); s != "False" || r != "NoCandidates" {
		t.Errorf("Satisfiable = %s/%s, want False/NoCandidates", s, r)
	}
}

func TestLabelValueTruncation(t *testing.T) {
	if got := labelValue("short-name"); got != "short-name" {
		t.Errorf("short names must pass through, got %q", got)
	}
	// 73 chars; the 63-char cut lands on trailing '-'/'.' separators, which a
	// valid label value must not end with.
	long := strings.Repeat("a", 60) + "-.-" + strings.Repeat("b", 10)
	got := labelValue(long)
	if len(got) > 63 {
		t.Fatalf("labelValue length %d > 63: %q", len(got), got)
	}
	if got == "" {
		t.Fatal("labelValue must not be empty")
	}
	if last := got[len(got)-1]; last == '-' || last == '_' || last == '.' {
		t.Errorf("labelValue must not end in a separator: %q", got)
	}
}

func TestReconcileNvidiaTarget(t *testing.T) {
	mc := reconcileMC("Qwen/Qwen3-30B-A3B")
	mc.Spec.DeviceClassName = "mig.nvidia.com"
	c, _, cs := newTestController(t, &fakeResolver{bounds: testBounds()}, mc)

	if err := c.reconcile(context.Background(), "team-a/qwen3"); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	tpl, err := cs.ResourceV1().ResourceClaimTemplates("team-a").Get(context.Background(), "qwen3", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("template not created: %v", err)
	}
	expr := tpl.Spec.Spec.Devices.Requests[0].Exactly.Selectors[0].CEL.Expression
	if !strings.Contains(expr, "device.attributes['gpu.nvidia.com']") {
		t.Errorf("NVIDIA-target template must compile against gpu.nvidia.com:\n%s", expr)
	}
	if tpl.Spec.Spec.Devices.Requests[0].Exactly.DeviceClassName != "mig.nvidia.com" {
		t.Errorf("deviceClassName = %s", tpl.Spec.Spec.Devices.Requests[0].Exactly.DeviceClassName)
	}
	if tpl.Annotations["llmfit.ai/target-driver"] != NvidiaDriverDomain {
		t.Errorf("target-driver annotation = %q", tpl.Annotations["llmfit.ai/target-driver"])
	}
	if tpl.Annotations["llmfit.ai/nvidia-boards-version"] == "" {
		t.Error("boards-version annotation missing on NVIDIA-target template")
	}

	// No NVIDIA slices in the informer: Satisfiable must go False with the
	// install hint, not crash or count llmfit devices.
	_, reason := condition(t, mustDyn(t, c), "team-a", "qwen3", condSatisfiable)
	if reason != apiv1alpha1.ReasonNoCandidates {
		t.Errorf("Satisfiable reason = %s, want NoCandidates", reason)
	}
}

// mustDyn extracts the fake dynamic client back out of the controller for
// status assertions.
func mustDyn(t *testing.T, c *Controller) *dynfake.FakeDynamicClient {
	t.Helper()
	dyn, ok := c.dyn.(*dynfake.FakeDynamicClient)
	if !ok {
		t.Fatal("controller not built on the fake dynamic client")
	}
	return dyn
}

func TestNvidiaTargetsPresent(t *testing.T) {
	mc := reconcileMC("Qwen/Qwen3-30B-A3B")
	c, _, _ := newTestController(t, &fakeResolver{bounds: testBounds()}, mc)
	if c.nvidiaTargetsPresent() {
		t.Error("llmfit-class claim must not flag nvidia targets")
	}

	nvMC := reconcileMC("Qwen/Qwen3-30B-A3B")
	nvMC.Name = "qwen3-nv"
	nvMC.Spec.DeviceClassName = "mig.nvidia.com"
	if err := c.mcInformer.GetStore().Add(mustUnstructured(t, nvMC)); err != nil {
		t.Fatalf("seeding informer store: %v", err)
	}
	if !c.nvidiaTargetsPresent() {
		t.Error("nvidia-class claim must flag nvidia targets (slice/claim gating rides on this)")
	}
}
