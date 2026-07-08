package modelclaim

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	apiv1alpha1 "github.com/sympozium-ai/llmfit-dra/api/v1alpha1"
)

func testBounds() *Bounds {
	return &Bounds{
		Model:           "Qwen/Qwen3.6-30B-A3B",
		ClaimName:       "qwen-qwen3-6-30b-a3b-fit",
		Quant:           "Q4_K_M",
		WeightsGb:       17.6,
		MemoryGi:        18,
		MinBandwidthGBs: 160,
		MinTps:          20,
		EfficiencyPct:   55,
		ResolverVersion: "0.9.37",
	}
}

// FitCEL must stay in lockstep with llmfit-core claim.rs render(): guarded
// membership checks so missing attributes are non-matches, not CEL errors.
func TestFitCELGolden(t *testing.T) {
	cel := FitCEL(testBounds(), DriverDomain)
	for _, want := range []string{
		"'memory' in device.capacity['llmfit.ai']",
		"device.capacity['llmfit.ai'].memory.compareTo(quantity('18Gi')) >= 0",
		"'memoryBandwidthGBs' in device.attributes['llmfit.ai']",
		"device.attributes['llmfit.ai'].memoryBandwidthGBs >= 160",
		"'healthy' in device.attributes['llmfit.ai']",
		"device.attributes['llmfit.ai'].healthy",
	} {
		if !strings.Contains(cel, want) {
			t.Errorf("FitCEL missing %q in:\n%s", want, cel)
		}
	}
}

func TestFitCELCPUClassWaivesBandwidth(t *testing.T) {
	// CPU devices publish no memoryBandwidthGBs; the cpu class would be
	// structurally unsatisfiable if the fit CEL demanded it. Memory and
	// health still hold.
	cel := FitCEL(testBounds(), "cpu.llmfit.ai")
	if strings.Contains(cel, "memoryBandwidthGBs") {
		t.Errorf("cpu-class fit CEL must not require bandwidth:\n%s", cel)
	}
	for _, want := range []string{
		"device.capacity['llmfit.ai'].memory.compareTo(quantity('18Gi')) >= 0",
		"device.attributes['llmfit.ai'].healthy",
	} {
		if !strings.Contains(cel, want) {
			t.Errorf("cpu-class fit CEL missing %q in:\n%s", want, cel)
		}
	}
}

func testMC() *apiv1alpha1.ModelClaim {
	return &apiv1alpha1.ModelClaim{
		TypeMeta: metav1.TypeMeta{
			APIVersion: apiv1alpha1.GroupVersion.String(),
			Kind:       apiv1alpha1.ModelClaimKind,
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "qwen36",
			Namespace: "team-a",
			UID:       "uid-1",
		},
	}
}

func TestBuildTemplateShape(t *testing.T) {
	tpl := BuildTemplate(testMC(), testBounds(), "gpu.llmfit.ai", []string{"device.attributes['llmfit.ai'].unifiedMemory"})

	if tpl.Name != "qwen36" || tpl.Namespace != "team-a" {
		t.Fatalf("template must share the ModelClaim's name/namespace, got %s/%s", tpl.Namespace, tpl.Name)
	}
	if len(tpl.OwnerReferences) != 1 || tpl.OwnerReferences[0].Kind != "ModelClaim" || !*tpl.OwnerReferences[0].Controller {
		t.Fatalf("template must be controller-owned by the ModelClaim: %+v", tpl.OwnerReferences)
	}
	reqs := tpl.Spec.Spec.Devices.Requests
	if len(reqs) != 1 || reqs[0].Name != "model" {
		t.Fatalf("want one request named 'model', got %+v", reqs)
	}
	if reqs[0].Exactly.DeviceClassName != "gpu.llmfit.ai" {
		t.Errorf("deviceClassName = %s", reqs[0].Exactly.DeviceClassName)
	}
	// Generated fit CEL first, extraSelectors appended (DRA ANDs selectors).
	if n := len(reqs[0].Exactly.Selectors); n != 2 {
		t.Fatalf("want 2 selectors (fit + extra), got %d", n)
	}
	if !strings.Contains(reqs[0].Exactly.Selectors[0].CEL.Expression, "memoryBandwidthGBs >= 160") {
		t.Errorf("first selector must be the fit CEL")
	}
	if reqs[0].Exactly.Selectors[1].CEL.Expression != "device.attributes['llmfit.ai'].unifiedMemory" {
		t.Errorf("extraSelector not passed through")
	}
	if tpl.Annotations["llmfit.ai/resolver-version"] != "0.9.37" {
		t.Errorf("resolver version annotation missing")
	}
}

func TestTemplateNeedsUpdate(t *testing.T) {
	desired := BuildTemplate(testMC(), testBounds(), "llmfit.ai", nil)
	same := BuildTemplate(testMC(), testBounds(), "llmfit.ai", nil)
	if TemplateNeedsUpdate(same, desired) {
		t.Error("identical templates must not need update")
	}
	changed := testBounds()
	changed.MinBandwidthGBs = 640
	if !TemplateNeedsUpdate(BuildTemplate(testMC(), changed, "llmfit.ai", nil), desired) {
		t.Error("bandwidth change must need update")
	}
}

func device(name, kind string, memGi int64, bwGBs int64, healthy bool, vendorManaged bool) resourceapi.Device {
	attrs := map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
		"kind":    {StringValue: ptr.To(kind)},
		"healthy": {BoolValue: ptr.To(healthy)},
	}
	if bwGBs > 0 {
		attrs["memoryBandwidthGBs"] = resourceapi.DeviceAttribute{IntValue: ptr.To(bwGBs)}
	}
	if vendorManaged {
		attrs["vendorManaged"] = resourceapi.DeviceAttribute{BoolValue: ptr.To(true)}
	}
	d := resourceapi.Device{Name: name, Attributes: attrs}
	if memGi > 0 {
		d.Capacity = map[resourceapi.QualifiedName]resourceapi.DeviceCapacity{
			"memory": {Value: *resource.NewQuantity(memGi*1024*1024*1024, resource.BinarySI)},
		}
	}
	return d
}

func slice(node string, devices ...resourceapi.Device) *resourceapi.ResourceSlice {
	return &resourceapi.ResourceSlice{
		ObjectMeta: metav1.ObjectMeta{Name: node + "-slice"},
		Spec: resourceapi.ResourceSliceSpec{
			Driver:   DriverDomain,
			Pool:     resourceapi.ResourcePool{Name: node},
			NodeName: ptr.To(node),
			Devices:  devices,
		},
	}
}

func TestEvaluateSlicesSatisfied(t *testing.T) {
	slices := []*resourceapi.ResourceSlice{
		slice("node-a", device("gpu0", "gpu", 24, 936, true, false), device("cpu0", "cpu", 64, 0, true, false)),
		slice("node-b", device("gpu0", "gpu", 24, 936, true, false)),
	}
	c := EvaluateSlices(slices, nil, testBounds(), "llmfit.ai")
	if c.Devices != 2 || c.Nodes != 2 {
		t.Fatalf("want 2 devices on 2 nodes, got %+v", c)
	}
}

func TestEvaluateSlicesShortfall(t *testing.T) {
	// Strix-class iGPU: plenty of memory, bandwidth 256 < 640.
	b := testBounds()
	b.MinBandwidthGBs = 640
	slices := []*resourceapi.ResourceSlice{
		slice("strix", device("gpu0", "gpu", 96, 256, true, false)),
	}
	c := EvaluateSlices(slices, nil, b, "llmfit.ai")
	if c.Devices != 0 {
		t.Fatalf("want no candidates, got %+v", c)
	}
	for _, want := range []string{"gpu0", "strix", "bandwidth 256 < 640"} {
		if !strings.Contains(c.Shortfall, want) {
			t.Errorf("shortfall %q missing %q", c.Shortfall, want)
		}
	}
}

func TestEvaluateSlicesCPUOnlyNoBandwidth(t *testing.T) {
	// The CI kind cluster shape: cpu0 has memory but publishes no bandwidth.
	slices := []*resourceapi.ResourceSlice{
		slice("kind-node", device("cpu0", "cpu", 64, 0, true, false)),
	}
	c := EvaluateSlices(slices, nil, testBounds(), "llmfit.ai")
	if c.Devices != 0 {
		t.Fatalf("want no candidates, got %+v", c)
	}
	if !strings.Contains(c.Shortfall, "no memoryBandwidthGBs published") {
		t.Errorf("shortfall = %q", c.Shortfall)
	}
}

func TestEvaluateSlicesExclusions(t *testing.T) {
	slices := []*resourceapi.ResourceSlice{
		slice("n1",
			device("gpu0", "gpu", 24, 936, true, true),   // vendorManaged: excluded from default classes
			device("gpu1", "gpu", 24, 936, false, false), // unhealthy
			device("npu0", "npu", 24, 936, true, false),  // wrong kind for gpu class
		),
	}
	if c := EvaluateSlices(slices, nil, testBounds(), "gpu.llmfit.ai"); c.Devices != 0 {
		t.Fatalf("want 0 candidates for gpu class, got %+v", c)
	}
	// The npu class does match npu0.
	if c := EvaluateSlices(slices, nil, testBounds(), "npu.llmfit.ai"); c.Devices != 1 {
		t.Fatalf("want npu candidate, got %+v", c)
	}
}

// Allocation-aware availability (issue #21): physics-satisfiable devices
// held by allocated claims are counted but not available.
func TestEvaluateSlicesSubtractsAllocated(t *testing.T) {
	slices := []*resourceapi.ResourceSlice{
		slice("node-a", device("gpu0", "gpu", 24, 936, true, false)),
		slice("node-b", device("gpu0", "gpu", 24, 936, true, false)),
	}
	// node-a's gpu0 is held; slice() names pools after the node.
	setPool := func(s *resourceapi.ResourceSlice, pool string) *resourceapi.ResourceSlice {
		s.Spec.Pool.Name = pool
		return s
	}
	setPool(slices[0], "node-a")
	setPool(slices[1], "node-b")
	allocated := map[string]string{"node-a/gpu0": "default/other-claim"}

	c := EvaluateSlices(slices, allocated, testBounds(), "llmfit.ai")
	if c.Devices != 2 || c.Available != 1 {
		t.Fatalf("want 2 devices / 1 available, got %+v", c)
	}

	allocated["node-b/gpu0"] = "team-a/another"
	c = EvaluateSlices(slices, allocated, testBounds(), "llmfit.ai")
	if c.Devices != 2 || c.Available != 0 || c.HeldBy == "" {
		t.Fatalf("want 2 devices / 0 available with a named holder, got %+v", c)
	}
}

func TestAllocatedDevices(t *testing.T) {
	rc := &resourceapi.ResourceClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "demo", Namespace: "default"},
		Status: resourceapi.ResourceClaimStatus{
			Allocation: &resourceapi.AllocationResult{
				Devices: resourceapi.DeviceAllocationResult{
					Results: []resourceapi.DeviceRequestAllocationResult{
						{Driver: DriverDomain, Pool: "node-a", Device: "gpu0"},
						{Driver: "other.vendor", Pool: "node-a", Device: "gpuX"},
					},
				},
			},
		},
	}
	held := AllocatedDevices([]*resourceapi.ResourceClaim{rc, {}})
	if len(held) != 1 || held["node-a/gpu0"] != "default/demo" {
		t.Fatalf("want only our driver's device held, got %v", held)
	}
}

// ExecResolver against a stub llmfit binary (shell script) — proves arg
// wiring and JSON parsing without the real binary.
func TestExecResolver(t *testing.T) {
	dir := t.TempDir()
	stub := filepath.Join(dir, "llmfit")
	script := `#!/bin/sh
echo "$@" > "` + dir + `/args"
cat <<'EOF'
{"model":"Qwen/Qwen3.6-30B-A3B","claimName":"qwen-fit","quant":"Q4_K_M",
 "weightsGb":17.6,"memoryGi":18,"minBandwidthGBs":160,"minTps":20.0,
 "efficiencyPct":55,"deviceClass":"llmfit.ai","resolverVersion":"0.9.37"}
EOF`
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	r := &ExecResolver{Bin: stub}
	b, err := r.Resolve(context.Background(), "Qwen/Qwen3.6-30B-A3B", 20, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if b.MemoryGi != 18 || b.MinBandwidthGBs != 160 || b.ResolverVersion != "0.9.37" {
		t.Fatalf("parsed bounds wrong: %+v", b)
	}
	args, _ := os.ReadFile(filepath.Join(dir, "args"))
	for _, want := range []string{"--json", "claim", "Qwen/Qwen3.6-30B-A3B", "--min-tps 20"} {
		if !strings.Contains(string(args), want) {
			t.Errorf("args %q missing %q", string(args), want)
		}
	}
}

func TestExecResolverError(t *testing.T) {
	dir := t.TempDir()
	stub := filepath.Join(dir, "llmfit")
	if err := os.WriteFile(stub, []byte("#!/bin/sh\necho 'Error: no model found' >&2\nexit 1"), 0o755); err != nil {
		t.Fatal(err)
	}
	r := &ExecResolver{Bin: stub}
	if _, err := r.Resolve(context.Background(), "nope", 20, "", 0); err == nil {
		t.Fatal("want error from failing resolver")
	} else if !strings.Contains(err.Error(), "no model found") {
		t.Errorf("stderr not surfaced: %v", err)
	}
}
