package probe

import (
	"os"
	"path/filepath"
	"testing"
)

// buildFakeTree constructs a minimal sysfs/procfs replica:
//   - one Intel iGPU (unified memory, xe driver) at pci 0000:00:02.0
//   - one AMD dGPU with dedicated VRAM at pci 0000:01:00.0
//   - one Intel NPU at pci 0000:00:0b.0
//   - connector entries and render nodes that must be skipped
func buildFakeTree(t *testing.T) (sysRoot, procRoot string) {
	t.Helper()
	root := t.TempDir()
	sysRoot = filepath.Join(root, "sys")
	procRoot = filepath.Join(root, "proc")

	type dev struct {
		class, name, pciAddr, vendor, device, driver string
		vram                                         string
		render                                       string
	}
	devs := []dev{
		{"drm", "card0", "0000:00:02.0", "0x8086", "0x64a0", "xe", "", "renderD128"},
		{"drm", "card1", "0000:01:00.0", "0x1002", "0x744c", "amdgpu", "25753026560", "renderD129"},
		{"accel", "accel0", "0000:00:0b.0", "0x8086", "0x643e", "intel_vpu", "", ""},
	}
	for _, d := range devs {
		// Real device dir lives under /sys/devices/pci0000:00/<addr>
		realDev := filepath.Join(sysRoot, "devices", "pci0000:00", d.pciAddr)
		mustMkdir(t, realDev)
		mustWrite(t, filepath.Join(realDev, "vendor"), d.vendor+"\n")
		mustWrite(t, filepath.Join(realDev, "device"), d.device+"\n")
		if d.vram != "" {
			mustWrite(t, filepath.Join(realDev, "mem_info_vram_total"), d.vram+"\n")
		}
		// The device dir's drm/ subdir pairs card and render minors.
		if d.class == "drm" {
			mustMkdir(t, filepath.Join(realDev, "drm", d.name))
			if d.render != "" {
				mustMkdir(t, filepath.Join(realDev, "drm", d.render))
			}
		}
		driverDir := filepath.Join(sysRoot, "bus", "pci", "drivers", d.driver)
		mustMkdir(t, driverDir)
		if err := os.Symlink(driverDir, filepath.Join(realDev, "driver")); err != nil && !os.IsExist(err) {
			t.Fatal(err)
		}
		// Class entry symlinks its "device" to the real dir, like sysfs does.
		classEntry := filepath.Join(sysRoot, "class", d.class, d.name)
		mustMkdir(t, classEntry)
		if err := os.Symlink(realDev, filepath.Join(classEntry, "device")); err != nil {
			t.Fatal(err)
		}
	}
	// Entries the walk must skip.
	mustMkdir(t, filepath.Join(sysRoot, "class", "drm", "card0-eDP-1"))
	mustMkdir(t, filepath.Join(sysRoot, "class", "drm", "renderD128"))
	mustMkdir(t, filepath.Join(sysRoot, "class", "drm", "version"))

	mustMkdir(t, procRoot)
	mustWrite(t, filepath.Join(procRoot, "meminfo"), "MemTotal:       32319904 kB\nMemFree:        10000000 kB\n")
	mustWrite(t, filepath.Join(procRoot, "cpuinfo"), "processor\t: 0\nmodel name\t: Intel(R) Core(TM) Ultra 7 258V\n")
	return sysRoot, procRoot
}

func TestWalk(t *testing.T) {
	sysRoot, procRoot := buildFakeTree(t)
	devices, err := New(sysRoot, procRoot).Walk()
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	// 2 GPUs + 1 NPU + 1 CPU
	if len(devices) != 4 {
		t.Fatalf("expected 4 devices, got %d: %+v", len(devices), devices)
	}

	byName := map[string]Device{}
	for _, d := range devices {
		byName[d.Name()] = d
	}

	igpu, ok := byName["gpu-0000-00-02-0"]
	if !ok {
		t.Fatal("gpu-0000-00-02-0 missing")
	}
	if igpu.PCIVendor != "8086" || igpu.PCIDevice != "64a0" {
		t.Errorf("gpu-0000-00-02-0 ids = %s:%s, want 8086:64a0", igpu.PCIVendor, igpu.PCIDevice)
	}
	if igpu.Driver != "xe" {
		t.Errorf("gpu-0000-00-02-0 driver = %q, want xe", igpu.Driver)
	}
	if igpu.VRAMBytes != 0 {
		t.Errorf("iGPU should detect no dedicated VRAM, got %d", igpu.VRAMBytes)
	}
	if igpu.PCIAddr != "0000:00:02.0" || igpu.PCIeRoot != "pci0000:00" {
		t.Errorf("gpu-0000-00-02-0 pci = %s / %s, want 0000:00:02.0 / pci0000:00", igpu.PCIAddr, igpu.PCIeRoot)
	}
	if igpu.DevNode != "/dev/dri/card0" || igpu.RenderNode != "/dev/dri/renderD128" {
		t.Errorf("gpu-0000-00-02-0 nodes = %s / %s, want /dev/dri/card0 / /dev/dri/renderD128", igpu.DevNode, igpu.RenderNode)
	}

	dgpu, ok := byName["gpu-0000-01-00-0"]
	if !ok {
		t.Fatal("gpu-0000-01-00-0 missing")
	}
	if dgpu.VRAMBytes != 25753026560 {
		t.Errorf("gpu-0000-01-00-0 vram = %d, want 25753026560", dgpu.VRAMBytes)
	}
	if dgpu.RenderNode != "/dev/dri/renderD129" {
		t.Errorf("gpu-0000-01-00-0 render node = %q, want /dev/dri/renderD129 (must pair via sysfs drm/, not card index)", dgpu.RenderNode)
	}

	npu, ok := byName["npu-0000-00-0b-0"]
	if !ok {
		t.Fatal("npu-0000-00-0b-0 missing")
	}
	if npu.Driver != "intel_vpu" {
		t.Errorf("npu-0000-00-0b-0 driver = %q, want intel_vpu", npu.Driver)
	}
	if npu.DevNode != "/dev/accel/accel0" || npu.RenderNode != "" {
		t.Errorf("npu-0000-00-0b-0 nodes = %s / %q, want /dev/accel/accel0 / \"\"", npu.DevNode, npu.RenderNode)
	}

	cpu, ok := byName["cpu0"]
	if !ok {
		t.Fatal("cpu0 missing")
	}
	if cpu.CPUModel != "Intel(R) Core(TM) Ultra 7 258V" {
		t.Errorf("cpu model = %q", cpu.CPUModel)
	}
	if cpu.SystemRAMBytes != 32319904*1024 {
		t.Errorf("ram = %d, want %d", cpu.SystemRAMBytes, uint64(32319904*1024))
	}
	if cpu.DevNode != "" || cpu.RenderNode != "" {
		t.Errorf("cpu0 should have no device nodes, got %s / %s", cpu.DevNode, cpu.RenderNode)
	}
}

// TestNameStableUnderRemoval is the audit's keystone fix: device names must
// identify silicon, not enumeration position. Removing an earlier card must
// not rename the survivors (gpu0/gpu1 counters would).
func TestNameStableUnderRemoval(t *testing.T) {
	sysRoot, procRoot := buildFakeTree(t)
	p := New(sysRoot, procRoot)
	before, err := p.Walk()
	if err != nil {
		t.Fatal(err)
	}
	// Hot-remove the first GPU (card0 / 0000:00:02.0).
	for _, path := range []string{
		filepath.Join(sysRoot, "class", "drm", "card0"),
		filepath.Join(sysRoot, "devices", "pci0000:00", "0000:00:02.0"),
	} {
		if err := os.RemoveAll(path); err != nil {
			t.Fatal(err)
		}
	}
	after, err := p.Walk()
	if err != nil {
		t.Fatal(err)
	}
	names := func(ds []Device) map[string]string {
		m := map[string]string{}
		for _, d := range ds {
			m[d.Name()] = d.PCIAddr
		}
		return m
	}
	b, a := names(before), names(after)
	if _, still := a["gpu-0000-00-02-0"]; still {
		t.Error("removed device still present")
	}
	if b["gpu-0000-01-00-0"] != "0000:01:00.0" || a["gpu-0000-01-00-0"] != "0000:01:00.0" {
		t.Errorf("surviving GPU renamed or remapped: before=%v after=%v", b, a)
	}
}

func TestHealthy(t *testing.T) {
	cases := []struct {
		name   string
		dev    Device
		want   bool
		reason string
	}{
		{"cpu always healthy", Device{Kind: KindCPU}, true, ""},
		{"bound gpu healthy", Device{Kind: KindGPU, Driver: "amdgpu"}, true, ""},
		{"unbound driver", Device{Kind: KindGPU}, false, "driverUnbound"},
		{"ecc errors", Device{Kind: KindGPU, Driver: "amdgpu", RASUncorrectable: 2}, false, "uncorrectableEcc"},
		{"npu unbound", Device{Kind: KindNPU}, false, "driverUnbound"},
	}
	for _, c := range cases {
		ok, reason := c.dev.Healthy()
		if ok != c.want || reason != c.reason {
			t.Errorf("%s: Healthy() = (%v, %q), want (%v, %q)", c.name, ok, reason, c.want, c.reason)
		}
	}
}

func TestWalkReadsRASCounter(t *testing.T) {
	sysRoot, procRoot := buildFakeTree(t)
	// Give the AMD dGPU an uncorrectable error count.
	mustMkdir(t, filepath.Join(sysRoot, "devices", "pci0000:00", "0000:01:00.0", "ras"))
	mustWrite(t, filepath.Join(sysRoot, "devices", "pci0000:00", "0000:01:00.0", "ras", "ue_count"), "3\n")
	devices, err := New(sysRoot, procRoot).Walk()
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range devices {
		if d.Name() == "gpu-0000-01-00-0" {
			if d.RASUncorrectable != 3 {
				t.Errorf("gpu-0000-01-00-0 RASUncorrectable = %d, want 3", d.RASUncorrectable)
			}
			if ok, reason := d.Healthy(); ok || reason != "uncorrectableEcc" {
				t.Errorf("gpu-0000-01-00-0 Healthy() = (%v, %q), want (false, uncorrectableEcc)", ok, reason)
			}
			return
		}
	}
	t.Fatal("gpu-0000-01-00-0 missing")
}

func TestWalkGPUWithoutRenderNode(t *testing.T) {
	sysRoot, procRoot := buildFakeTree(t)
	// A display-only DRM device: drm/ pairs only the card minor.
	realDev := filepath.Join(sysRoot, "devices", "pci0000:00", "0000:02:00.0")
	mustMkdir(t, filepath.Join(realDev, "drm", "card2"))
	mustWrite(t, filepath.Join(realDev, "vendor"), "0x1a03\n")
	mustWrite(t, filepath.Join(realDev, "device"), "0x2000\n")
	classEntry := filepath.Join(sysRoot, "class", "drm", "card2")
	mustMkdir(t, classEntry)
	if err := os.Symlink(realDev, filepath.Join(classEntry, "device")); err != nil {
		t.Fatal(err)
	}

	devices, err := New(sysRoot, procRoot).Walk()
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range devices {
		if d.Name() == "gpu-0000-02-00-0" {
			if d.DevNode != "/dev/dri/card2" || d.RenderNode != "" {
				t.Errorf("gpu2 nodes = %s / %q, want /dev/dri/card2 / \"\"", d.DevNode, d.RenderNode)
			}
			return
		}
	}
	t.Fatal("gpu2 missing")
}

func TestWalkStableAcrossRuns(t *testing.T) {
	sysRoot, procRoot := buildFakeTree(t)
	p := New(sysRoot, procRoot)
	a, err := p.Walk()
	if err != nil {
		t.Fatal(err)
	}
	b, err := p.Walk()
	if err != nil {
		t.Fatal(err)
	}
	if len(a) != len(b) {
		t.Fatalf("walk not stable: %d vs %d devices", len(a), len(b))
	}
	for i := range a {
		if a[i] != b[i] {
			t.Errorf("device %d differs across runs: %+v vs %+v", i, a[i], b[i])
		}
	}
}

func TestWalkMissingClassesIsNotAnError(t *testing.T) {
	root := t.TempDir()
	procRoot := filepath.Join(root, "proc")
	mustMkdir(t, procRoot)
	mustWrite(t, filepath.Join(procRoot, "meminfo"), "MemTotal: 1024 kB\n")
	mustWrite(t, filepath.Join(procRoot, "cpuinfo"), "model name: test\n")

	devices, err := New(filepath.Join(root, "sys"), procRoot).Walk()
	if err != nil {
		t.Fatalf("Walk on empty sysfs: %v", err)
	}
	if len(devices) != 1 || devices[0].Kind != KindCPU {
		t.Fatalf("expected only cpu fallback, got %+v", devices)
	}
}

func mustMkdir(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestVRAMFromIntelLmem(t *testing.T) {
	sysRoot, procRoot := buildFakeTree(t)
	// Intel discrete (i915): lmem_total_bytes lives in the CARD dir.
	realDev := filepath.Join(sysRoot, "devices", "pci0000:00", "0000:03:00.0")
	mustMkdir(t, filepath.Join(realDev, "drm", "card3"))
	mustWrite(t, filepath.Join(realDev, "vendor"), "0x8086\n")
	mustWrite(t, filepath.Join(realDev, "device"), "0x56a0\n")
	classEntry := filepath.Join(sysRoot, "class", "drm", "card3")
	mustMkdir(t, classEntry)
	if err := os.Symlink(realDev, filepath.Join(classEntry, "device")); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(classEntry, "lmem_total_bytes"), "17179869184\n")

	devices, err := New(sysRoot, procRoot).Walk()
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range devices {
		if d.Name() == "gpu-0000-03-00-0" {
			if d.VRAMBytes != 17179869184 {
				t.Errorf("i915 lmem vram = %d, want 17179869184", d.VRAMBytes)
			}
			return
		}
	}
	t.Fatal("intel dGPU missing")
}
