package modelclaim

import (
	"context"
	"strings"
	"testing"

	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	dracel "k8s.io/dynamic-resource-allocation/cel"
	"k8s.io/utils/ptr"

	"github.com/sympozium-ai/llmfit-dra/internal/index"
)

// nvidiaBounds is the worked example from the design: a model needing 24Gi
// of weights and 644 GB/s of derived bandwidth. On an A100-80GB that admits
// 3g.40gb and larger; 2g.20gb (derived 510 GB/s) must be rejected.
func nvidiaBounds() *Bounds {
	b := testBounds()
	b.MemoryGi = 24
	b.MinBandwidthGBs = 644
	return b
}

func TestMigThresholdMiB(t *testing.T) {
	boards := testBoards(t)
	cases := []struct {
		product string
		wantThr int64
		wantOK  bool
	}{
		// peak 2039, 8 slices: k=3, midpoint 2.5/8 of 80Gi = 25600Mi > 24Gi floor.
		{"NVIDIA A100-SXM4-80GB", 25600, true},
		// peak 1555: k=4, midpoint 17920Mi — memory floor 24576Mi dominates.
		{"NVIDIA A100-SXM4-40GB", 24576, true},
		// peak 3350: k=2, midpoint 15360Mi — memory floor dominates.
		{"NVIDIA H100 80GB HBM3", 24576, true},
		// peak 4800, 141GB board: k=2, midpoint 27072Mi dominates the floor.
		{"NVIDIA H200", 27072, true},
		// 4-slice board (24GB): k=3, midpoint 15360Mi; floor 24576Mi = whole board.
		{"NVIDIA A30", 24576, true},
		// Not MIG-capable: fail closed.
		{"NVIDIA L4", 0, false},
	}
	for _, tc := range cases {
		board, ok := boards.ByProductName(tc.product)
		if !ok {
			t.Fatalf("%s missing from board table", tc.product)
		}
		thr, ok := migThresholdMiB(board, nvidiaBounds())
		if ok != tc.wantOK || thr != tc.wantThr {
			t.Errorf("%s: got (%d, %v), want (%d, %v)", tc.product, thr, ok, tc.wantThr, tc.wantOK)
		}
	}

	// Bandwidth beyond the full board: no partition can ever satisfy.
	board, _ := boards.ByProductName("NVIDIA A100-SXM4-80GB")
	b := nvidiaBounds()
	b.MinBandwidthGBs = 2100
	if _, ok := migThresholdMiB(board, b); ok {
		t.Error("bandwidth above board peak must fail closed")
	}
	// Memory floor above the board: same.
	b = nvidiaBounds()
	b.MemoryGi = 200
	if _, ok := migThresholdMiB(board, b); ok {
		t.Error("memory floor above board capacity must fail closed")
	}
}

func TestMigMinSMs(t *testing.T) {
	boards := testBoards(t)
	a100, _ := boards.ByProductName("NVIDIA A100-SXM4-80GB")

	if sms, ok := migMinSMs(a100, 0); !ok || sms != 0 {
		t.Errorf("no compute floor must impose nothing, got (%d, %v)", sms, ok)
	}
	// 312 TFLOPS over 108 SMs: a 100-TFLOPS floor needs ceil(100*108/312)=35 SMs.
	if sms, ok := migMinSMs(a100, 100); !ok || sms != 35 {
		t.Errorf("compute floor 100: got (%d, %v), want (35, true)", sms, ok)
	}
	// Floor above the whole board: excluded.
	if _, ok := migMinSMs(a100, 400); ok {
		t.Error("compute floor above board TFLOPS must exclude the board")
	}
	// Boards without compute data are excluded when a floor is set — the
	// never-waived contradiction FitCEL also enforces.
	noData := index.NvidiaBoard{MemoryBandwidthGBs: 100, MemoryMiB: 1024, MemorySlices: 8}
	if _, ok := migMinSMs(noData, 10); ok {
		t.Error("compute floor with no board compute data must exclude")
	}
}

// The golden MIG expression for the worked example, per-board thresholds
// grouped: 24576Mi (floor-dominant boards), 25600Mi (A100-80/H100-PCIe),
// 27072Mi (H200). Non-MIG boards (A10, L4, L40S, T4) must not appear.
func TestNvidiaFitCELGoldenMIG(t *testing.T) {
	cel := NvidiaFitCEL(nvidiaBounds(), testBoards(t), "mig.nvidia.com", 0)
	want := "'type' in device.attributes['gpu.nvidia.com'] && " +
		"'productName' in device.attributes['gpu.nvidia.com'] && " +
		"'memory' in device.capacity['gpu.nvidia.com'] && " +
		"((device.attributes['gpu.nvidia.com'].type == 'mig' && (" +
		"(device.attributes['gpu.nvidia.com'].productName in ['NVIDIA A100-PCIE-40GB', 'NVIDIA A100-SXM4-40GB', 'NVIDIA A30', 'NVIDIA H100 80GB HBM3', 'NVIDIA H100 NVL'] && " +
		"device.capacity['gpu.nvidia.com'].memory.compareTo(quantity('24576Mi')) >= 0) || " +
		"(device.attributes['gpu.nvidia.com'].productName in ['NVIDIA A100 80GB PCIe', 'NVIDIA A100-PCIE-80GB', 'NVIDIA A100-SXM4-80GB', 'NVIDIA H100 PCIe'] && " +
		"device.capacity['gpu.nvidia.com'].memory.compareTo(quantity('25600Mi')) >= 0) || " +
		"(device.attributes['gpu.nvidia.com'].productName in ['NVIDIA H200'] && " +
		"device.capacity['gpu.nvidia.com'].memory.compareTo(quantity('27072Mi')) >= 0))))"
	if cel != want {
		t.Errorf("golden mismatch:\n got: %s\nwant: %s", cel, want)
	}
}

func TestNvidiaFitCELEmptyAdmissibleSet(t *testing.T) {
	b := nvidiaBounds()
	b.MinBandwidthGBs = 10000 // beyond every board
	if cel := NvidiaFitCEL(b, testBoards(t), "mig.nvidia.com", 0); cel != "false" {
		t.Errorf("unsatisfiable bounds must compile to 'false', got: %s", cel)
	}
}

func TestNvidiaFitCELComputeFloor(t *testing.T) {
	cel := NvidiaFitCEL(nvidiaBounds(), testBoards(t), "mig.nvidia.com", 100)
	for _, want := range []string{
		"'multiprocessors' in device.capacity['gpu.nvidia.com']",
		".multiprocessors.compareTo(quantity('",
	} {
		if !strings.Contains(cel, want) {
			t.Errorf("compute-floored CEL missing %q in:\n%s", want, cel)
		}
	}
	if strings.Contains(NvidiaFitCEL(nvidiaBounds(), testBoards(t), "mig.nvidia.com", 0), "multiprocessors") {
		t.Error("no compute floor must not emit a multiprocessors clause")
	}
}

// nvDevice builds a dracel.Device the way gpu.nvidia.com publishes them.
func nvDevice(devType, product, memory string, extra map[string]string) dracel.Device {
	attrs := map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{}
	if devType != "" {
		attrs["type"] = resourceapi.DeviceAttribute{StringValue: ptr.To(devType)}
	}
	if product != "" {
		attrs["productName"] = resourceapi.DeviceAttribute{StringValue: ptr.To(product)}
	}
	for k, v := range extra {
		attrs[resourceapi.QualifiedName(k)] = resourceapi.DeviceAttribute{StringValue: ptr.To(v)}
	}
	caps := map[resourceapi.QualifiedName]resourceapi.DeviceCapacity{}
	if memory != "" {
		caps["memory"] = resourceapi.DeviceCapacity{Value: resource.MustParse(memory)}
	}
	return dracel.Device{Driver: NvidiaDriverDomain, Attributes: attrs, Capacity: caps}
}

// The no-hardware correctness backstop: evaluate the generated expressions
// with the same CEL compiler the DRA scheduler plugin uses, against devices
// shaped like real gpu.nvidia.com slice dumps.
func TestNvidiaFitCELSemantic(t *testing.T) {
	boards := testBoards(t)
	compile := func(expr string) dracel.CompilationResult {
		t.Helper()
		res := dracel.GetCompiler(dracel.Features{}).CompileCELExpression(expr, dracel.Options{})
		if res.Error != nil {
			t.Fatalf("generated CEL does not compile: %v\n%s", res.Error, expr)
		}
		return res
	}
	migCEL := compile(NvidiaFitCEL(nvidiaBounds(), boards, "mig.nvidia.com", 0))
	gpuCEL := compile(NvidiaFitCEL(nvidiaBounds(), boards, "gpu.nvidia.com", 0))

	cases := []struct {
		name   string
		cel    dracel.CompilationResult
		dev    dracel.Device
		expect bool
	}{
		// Published MIG capacities run under nominal (reserved VRAM) — these
		// are realistic dump values, which is exactly what midpoints absorb.
		{"mig 1g.10gb rejected", migCEL, nvDevice("mig", "NVIDIA A100-SXM4-80GB", "9856Mi", map[string]string{"profile": "1g.10gb"}), false},
		{"mig 2g.20gb rejected (derived 510 < 644)", migCEL, nvDevice("mig", "NVIDIA A100-SXM4-80GB", "19968Mi", map[string]string{"profile": "2g.20gb"}), false},
		{"mig 3g.40gb admitted", migCEL, nvDevice("mig", "NVIDIA A100-SXM4-80GB", "40192Mi", map[string]string{"profile": "3g.40gb"}), true},
		{"mig 7g.80gb admitted", migCEL, nvDevice("mig", "NVIDIA A100-SXM4-80GB", "81050Mi", map[string]string{"profile": "7g.80gb"}), true},
		{"mig on A100-40GB: only full profile fits", migCEL, nvDevice("mig", "NVIDIA A100-SXM4-40GB", "20096Mi", map[string]string{"profile": "4g.20gb"}), false},
		{"mig 7g.40gb admitted on A100-40GB", migCEL, nvDevice("mig", "NVIDIA A100-SXM4-40GB", "40192Mi", map[string]string{"profile": "7g.40gb"}), true},
		{"H200 mig below derived threshold rejected", migCEL, nvDevice("mig", "NVIDIA H200", "26000Mi", nil), false},
		{"H200 mig above derived threshold admitted", migCEL, nvDevice("mig", "NVIDIA H200", "28000Mi", nil), true},
		{"unknown board fails closed", migCEL, nvDevice("mig", "NVIDIA B300", "40192Mi", nil), false},
		{"full gpu invisible to mig class", migCEL, nvDevice("gpu", "NVIDIA A100-SXM4-80GB", "80Gi", nil), false},
		{"missing productName is a non-match, not an error", migCEL, nvDevice("mig", "", "40192Mi", nil), false},

		{"gpu class admits full A100", gpuCEL, nvDevice("gpu", "NVIDIA A100-SXM4-80GB", "80Gi", nil), true},
		{"gpu class rejects A10 (600 < 644 GB/s board peak)", gpuCEL, nvDevice("gpu", "NVIDIA A10", "24Gi", nil), false},
		{"gpu class admits L40S (864 GB/s)", gpuCEL, nvDevice("gpu", "NVIDIA L40S", "48Gi", nil), true},
		{"mig invisible to gpu class", gpuCEL, nvDevice("mig", "NVIDIA A100-SXM4-80GB", "40192Mi", nil), false},
	}
	for _, tc := range cases {
		matches, _, err := tc.cel.DeviceMatches(context.Background(), tc.dev)
		if err != nil {
			t.Errorf("%s: evaluation error (guards must prevent this): %v", tc.name, err)
			continue
		}
		if matches != tc.expect {
			t.Errorf("%s: got %v, want %v", tc.name, matches, tc.expect)
		}
	}
}

func TestParseMIGProfile(t *testing.T) {
	cases := []struct {
		in     string
		g, gb  int64
		me, ok bool
	}{
		{"1g.5gb", 1, 5, false, true},
		{"3g.40gb", 3, 40, false, true},
		{"1g.10gb+me", 1, 10, true, true},
		{"7g.141gb", 7, 141, false, true},
		{"weird", 0, 0, false, false},
		{"", 0, 0, false, false},
	}
	for _, tc := range cases {
		g, gb, me, ok := parseMIGProfile(tc.in)
		if g != tc.g || gb != tc.gb || me != tc.me || ok != tc.ok {
			t.Errorf("parseMIGProfile(%q) = (%d,%d,%v,%v), want (%d,%d,%v,%v)",
				tc.in, g, gb, me, ok, tc.g, tc.gb, tc.me, tc.ok)
		}
	}
}

func TestBuildTemplateNvidiaTarget(t *testing.T) {
	boards := testBoards(t)
	mc := testMC()
	mc.Spec.DeviceClassName = "mig.nvidia.com"
	tpl := BuildTemplate(mc, nvidiaBounds(), "mig.nvidia.com", nil, boards)

	expr := tpl.Spec.Spec.Devices.Requests[0].Exactly.Selectors[0].CEL.Expression
	if !strings.Contains(expr, "device.attributes['gpu.nvidia.com']") {
		t.Errorf("NVIDIA-target template must read gpu.nvidia.com attributes:\n%s", expr)
	}
	if strings.Contains(expr, "'llmfit.ai'") {
		t.Errorf("NVIDIA-target CEL must not reference llmfit.ai attributes:\n%s", expr)
	}
	if tpl.Spec.Spec.Devices.Requests[0].Exactly.DeviceClassName != "mig.nvidia.com" {
		t.Error("deviceClassName must pass through to the request")
	}
	if got := tpl.Annotations["llmfit.ai/target-driver"]; got != NvidiaDriverDomain {
		t.Errorf("target-driver annotation = %q", got)
	}
	if tpl.Annotations["llmfit.ai/nvidia-boards-version"] != boards.Version() {
		t.Error("boards-version annotation must carry the table hash")
	}

	// llmfit-target templates must not gain the boards-version annotation —
	// board-table updates must not churn them.
	plain := BuildTemplate(testMC(), testBounds(), DriverDomain, nil, boards)
	if _, ok := plain.Annotations["llmfit.ai/nvidia-boards-version"]; ok {
		t.Error("llmfit-target template must not carry nvidia-boards-version")
	}
	if got := plain.Annotations["llmfit.ai/target-driver"]; got != DriverDomain {
		t.Errorf("llmfit target-driver annotation = %q", got)
	}
}

func TestBackendFor(t *testing.T) {
	cases := []struct {
		class, target string
		want          backend
	}{
		{"llmfit.ai", "", backendLlmfit},
		{"gpu.llmfit.ai", "", backendLlmfit},
		{"cpu.llmfit.ai", "llmfit.ai", backendLlmfit},
		{"gpu.nvidia.com", "", backendNvidia},
		{"mig.nvidia.com", "", backendNvidia},
		{"my-custom-class", "gpu.nvidia.com", backendNvidia},
		{"my-custom-class", "", backendLlmfit},
	}
	for _, tc := range cases {
		if got := backendFor(tc.class, tc.target); got != tc.want {
			t.Errorf("backendFor(%q, %q) = %v, want %v", tc.class, tc.target, got, tc.want)
		}
	}
}
