package publisher

import (
	"testing"

	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/sympozium-ai/llmfit-dra/internal/index"
	"github.com/sympozium-ai/llmfit-dra/internal/llmfit"
	"github.com/sympozium-ai/llmfit-dra/internal/probe"
)

type (
	k8sQualifiedName   = resourceapi.QualifiedName
	k8sDeviceAttribute = resourceapi.DeviceAttribute
	k8sDevice          = resourceapi.Device
)

const systemRAM = uint64(32 * 1024 * 1024 * 1024)

func testDevices() []probe.Device {
	return []probe.Device{
		{Kind: probe.KindGPU, Index: 0, PCIVendor: "8086", PCIDevice: "64a0", PCIAddr: "0000:00:02.0", PCIeRoot: "pci0000:00", Driver: "xe"},
		{Kind: probe.KindGPU, Index: 1, PCIVendor: "10de", PCIDevice: "2684", PCIAddr: "0000:01:00.0", PCIeRoot: "pci0000:00", Driver: "nvidia", VRAMBytes: 24 * 1024 * 1024 * 1024},
		{Kind: probe.KindNPU, Index: 0, PCIVendor: "8086", PCIDevice: "643e", PCIAddr: "0000:00:0b.0", PCIeRoot: "pci0000:00", Driver: "intel_vpu"},
		{Kind: probe.KindGPU, Index: 2, PCIVendor: "abcd", PCIDevice: "1234", Driver: "mystery"},
		{Kind: probe.KindCPU, Index: 0, CPUModel: "Intel(R) Core(TM) Ultra 7 258V", SystemRAMBytes: systemRAM},
	}
}

func mustIndex(t *testing.T) *index.Index {
	t.Helper()
	idx, err := index.Load()
	if err != nil {
		t.Fatal(err)
	}
	return idx
}

func TestBuildDevicesAttributeMapping(t *testing.T) {
	devices := BuildDevices(testDevices(), mustIndex(t), systemRAM, nil, Options{})
	if len(devices) != 5 {
		t.Fatalf("expected 5 devices, got %d", len(devices))
	}

	byName := map[string]int{}
	for i, d := range devices {
		byName[d.Name] = i
	}

	igpu := devices[byName["gpu0"]]
	assertStr(t, igpu.Attributes, "vendor", "intel")
	assertStr(t, igpu.Attributes, "model", "Intel Arc Graphics 140V")
	assertStr(t, igpu.Attributes, "kind", "gpu")
	assertStr(t, igpu.Attributes, "driver", "xe")
	assertStr(t, igpu.Attributes, "pcieRoot", "pci0000:00")
	assertStr(t, igpu.Attributes, "resource.kubernetes.io/pcieRoot", "pci0000:00")
	assertBool(t, igpu.Attributes, "unifiedMemory", true)
	assertBool(t, igpu.Attributes, "indexed", true)
	assertBool(t, igpu.Attributes, "healthy", true)
	assertInt(t, igpu.Attributes, "memoryBandwidthGBs", 136)
	// Unified memory device: capacity = system RAM.
	assertMemory(t, igpu, int64(systemRAM))

	dgpu := devices[byName["gpu1"]]
	assertStr(t, dgpu.Attributes, "vendor", "nvidia")
	assertStr(t, dgpu.Attributes, "model", "NVIDIA GeForce RTX 4090")
	assertBool(t, dgpu.Attributes, "unifiedMemory", false)
	assertInt(t, dgpu.Attributes, "memoryBandwidthGBs", 1008)
	// Dedicated VRAM: capacity = VRAM, not system RAM.
	assertMemory(t, dgpu, 24*1024*1024*1024)

	npu := devices[byName["npu0"]]
	assertStr(t, npu.Attributes, "kind", "npu")
	assertBool(t, npu.Attributes, "indexed", true)

	unknown := devices[byName["gpu2"]]
	assertBool(t, unknown.Attributes, "indexed", false)
	assertStr(t, unknown.Attributes, "vendor", "pci-abcd")
	assertStr(t, unknown.Attributes, "model", "pci-abcd-1234")
	if _, ok := unknown.Attributes["memoryBandwidthGBs"]; ok {
		t.Error("unknown device must not carry a bandwidth attribute")
	}

	cpu := devices[byName["cpu0"]]
	assertStr(t, cpu.Attributes, "vendor", "cpu")
	assertStr(t, cpu.Attributes, "model", "Intel(R) Core(TM) Ultra 7 258V")
	assertMemory(t, cpu, int64(systemRAM))
}

func TestBuildDevicesRespectsAttributeLimits(t *testing.T) {
	for _, d := range BuildDevices(testDevices(), mustIndex(t), systemRAM, nil, Options{}) {
		total := len(d.Attributes) + len(d.Capacity)
		if total > 32 {
			t.Errorf("device %s has %d attributes+capacities, exceeding the DRA limit of 32", d.Name, total)
		}
		for name, attr := range d.Attributes {
			if attr.StringValue != nil && len(*attr.StringValue) > 64 {
				t.Errorf("device %s attribute %s exceeds 64 chars", d.Name, name)
			}
		}
	}
}

func TestBuildResources(t *testing.T) {
	devices := BuildDevices(testDevices(), mustIndex(t), systemRAM, nil, Options{})
	res := BuildResources("carbon", devices)
	pool, ok := res.Pools["carbon"]
	if !ok {
		t.Fatal("pool named after node missing")
	}
	if len(pool.Slices) != 1 {
		t.Fatalf("expected 1 slice, got %d", len(pool.Slices))
	}
	if len(pool.Slices[0].Devices) != len(devices) {
		t.Fatalf("slice device count = %d, want %d", len(pool.Slices[0].Devices), len(devices))
	}
}

type attrs = map[k8sQualifiedName]k8sDeviceAttribute

func assertStr(t *testing.T, a attrs, key, want string) {
	t.Helper()
	attr, ok := a[k8sQualifiedName(key)]
	if !ok || attr.StringValue == nil {
		t.Errorf("attribute %q missing or not a string", key)
		return
	}
	if *attr.StringValue != want {
		t.Errorf("attribute %q = %q, want %q", key, *attr.StringValue, want)
	}
}

func assertBool(t *testing.T, a attrs, key string, want bool) {
	t.Helper()
	attr, ok := a[k8sQualifiedName(key)]
	if !ok || attr.BoolValue == nil {
		t.Errorf("attribute %q missing or not a bool", key)
		return
	}
	if *attr.BoolValue != want {
		t.Errorf("attribute %q = %v, want %v", key, *attr.BoolValue, want)
	}
}

func assertInt(t *testing.T, a attrs, key string, want int64) {
	t.Helper()
	attr, ok := a[k8sQualifiedName(key)]
	if !ok || attr.IntValue == nil {
		t.Errorf("attribute %q missing or not an int", key)
		return
	}
	if *attr.IntValue != want {
		t.Errorf("attribute %q = %d, want %d", key, *attr.IntValue, want)
	}
}

func assertMemory(t *testing.T, d k8sDevice, wantBytes int64) {
	t.Helper()
	cap, ok := d.Capacity[k8sQualifiedName("memory")]
	if !ok {
		t.Errorf("device %s has no memory capacity", d.Name)
		return
	}
	want := resource.NewQuantity(wantBytes, resource.BinarySI)
	if cap.Value.Cmp(*want) != 0 {
		t.Errorf("device %s memory = %s, want %s", d.Name, cap.Value.String(), want.String())
	}
}

func TestBuildDevicesWithLLMFit(t *testing.T) {
	bw := 256.0
	vram := 62.56
	sys := &llmfit.System{
		TotalRAMGB: 62.56,
		CPUName:    "AMD RYZEN AI MAX+ 395 w/ Radeon 8060S",
		HasGPU:     true,
		GPUs: []llmfit.GPU{{
			Name:                "AMD Radeon 8060S (Strix Halo)",
			VRAMGB:              &vram,
			Backend:             "Vulkan",
			Count:               1,
			UnifiedMemory:       true,
			MemoryBandwidthGBps: &bw,
		}},
	}
	probed := []probe.Device{
		{Kind: probe.KindGPU, Index: 0, PCIVendor: "1002", PCIDevice: "1586", PCIAddr: "0000:c3:00.0", PCIeRoot: "pci0000:00", Driver: "amdgpu", VRAMBytes: 64 * 1024 * 1024 * 1024},
		{Kind: probe.KindNPU, Index: 0, PCIVendor: "1022", PCIDevice: "17f0", Driver: "amdxdna"},
		{Kind: probe.KindCPU, Index: 0, CPUModel: "AMD RYZEN AI MAX+ 395 w/ Radeon 8060S", SystemRAMBytes: systemRAM},
	}
	devices := BuildDevices(probed, mustIndex(t), systemRAM, sys, Options{})

	byName := map[string]k8sDevice{}
	for _, d := range devices {
		byName[d.Name] = d
	}

	gpu := byName["gpu0"]
	assertStr(t, gpu.Attributes, "source", "llmfit")
	assertStr(t, gpu.Attributes, "model", "AMD Radeon 8060S (Strix Halo)")
	assertStr(t, gpu.Attributes, "backend", "Vulkan")
	assertInt(t, gpu.Attributes, "memoryBandwidthGBs", 256)
	assertBool(t, gpu.Attributes, "unifiedMemory", true)
	// llmfit's fit budget (shared pool) wins over the probe's VRAM carve-out.
	pool := 62.56 * float64(1<<30)
	assertMemory(t, gpu, int64(pool))
	// Identity still comes from the probe.
	assertStr(t, gpu.Attributes, "pciAddress", "0000:c3:00.0")
	assertStr(t, gpu.Attributes, "driver", "amdgpu")

	// llmfit does not report XDNA NPUs: falls back to the embedded index.
	npu := byName["npu0"]
	assertStr(t, npu.Attributes, "source", "index")
	assertStr(t, npu.Attributes, "model", "AMD XDNA 2 NPU (Strix Halo)")

	cpu := byName["cpu0"]
	assertStr(t, cpu.Attributes, "source", "llmfit")
	assertMemory(t, cpu, int64(pool))
}

func TestBuildDevicesLLMFitFallbackWhenNil(t *testing.T) {
	devices := BuildDevices(testDevices(), mustIndex(t), systemRAM, nil, Options{})
	byName := map[string]k8sDevice{}
	for _, d := range devices {
		byName[d.Name] = d
	}
	assertStr(t, byName["gpu0"].Attributes, "source", "index")
	assertStr(t, byName["gpu2"].Attributes, "source", "probe")
	assertStr(t, byName["cpu0"].Attributes, "source", "probe")
}

func TestBuildDevicesLLMFitBandwidthFallsBackToIndex(t *testing.T) {
	vram := 62.56
	sys := &llmfit.System{
		CPUName: "AMD RYZEN AI MAX+ 395 w/ Radeon 8060S",
		HasGPU:  true,
		// Stale pci.ids scenario: lspci resolved only the vendor, so llmfit
		// couldn't price bandwidth from the name.
		GPUs: []llmfit.GPU{{Name: "AMD/ATI", VRAMGB: &vram, Backend: "Vulkan", Count: 1, UnifiedMemory: true}},
	}
	probed := []probe.Device{
		{Kind: probe.KindGPU, Index: 0, PCIVendor: "1002", PCIDevice: "1586", Driver: "amdgpu"},
		{Kind: probe.KindCPU, Index: 0, SystemRAMBytes: systemRAM},
	}
	devices := BuildDevices(probed, mustIndex(t), systemRAM, sys, Options{})
	for _, d := range devices {
		if d.Name != "gpu0" {
			continue
		}
		assertStr(t, d.Attributes, "source", "llmfit")
		// Bandwidth rescued from the PCI-ID index (1002:1586 = Strix Halo).
		assertInt(t, d.Attributes, "memoryBandwidthGBs", 256)
		return
	}
	t.Fatal("gpu0 not built")
}

func TestBuildDevicesHealth(t *testing.T) {
	idx := mustIndex(t)
	devices := BuildDevices([]probe.Device{
		{Kind: probe.KindGPU, Index: 0, PCIVendor: "1002", PCIDevice: "744c", Driver: "amdgpu", RASUncorrectable: 1},
		{Kind: probe.KindGPU, Index: 1, PCIVendor: "8086", PCIDevice: "64a0"}, // no driver bound
		{Kind: probe.KindCPU, Index: 0, SystemRAMBytes: systemRAM},
	}, idx, systemRAM, nil, Options{Taints: true})

	byName := map[string]k8sDevice{}
	for _, d := range devices {
		byName[d.Name] = d
	}
	assertBool(t, byName["gpu0"].Attributes, "healthy", false)
	assertStr(t, byName["gpu0"].Attributes, "healthReason", "uncorrectableEcc")
	assertBool(t, byName["gpu1"].Attributes, "healthy", false)
	assertStr(t, byName["gpu1"].Attributes, "healthReason", "driverUnbound")
	assertBool(t, byName["cpu0"].Attributes, "healthy", true)
	if _, ok := byName["cpu0"].Attributes["healthReason"]; ok {
		t.Error("healthy device must not carry healthReason")
	}

	// Options{Taints: true}: unhealthy devices carry a NoSchedule taint…
	gpu0Taints := byName["gpu0"].Taints
	if len(gpu0Taints) != 1 || gpu0Taints[0].Key != "llmfit.ai/unhealthy" ||
		gpu0Taints[0].Value != "uncorrectableEcc" || gpu0Taints[0].Effect != resourceapi.DeviceTaintEffectNoSchedule {
		t.Errorf("gpu0 taints = %+v", gpu0Taints)
	}
	// …healthy devices never do.
	if len(byName["cpu0"].Taints) != 0 {
		t.Errorf("cpu0 must not be tainted, got %+v", byName["cpu0"].Taints)
	}
}

func TestBuildDevicesNoTaintsByDefault(t *testing.T) {
	devices := BuildDevices([]probe.Device{
		{Kind: probe.KindGPU, Index: 0, PCIVendor: "1002", PCIDevice: "744c", RASUncorrectable: 9},
	}, mustIndex(t), systemRAM, nil, Options{})
	if len(devices[0].Taints) != 0 {
		t.Errorf("taints published without opt-in: %+v", devices[0].Taints)
	}
}
