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
	}
	devs := []dev{
		{"drm", "card0", "0000:00:02.0", "0x8086", "0x64a0", "xe", ""},
		{"drm", "card1", "0000:01:00.0", "0x1002", "0x744c", "amdgpu", "25753026560"},
		{"accel", "accel0", "0000:00:0b.0", "0x8086", "0x643e", "intel_vpu", ""},
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

	igpu, ok := byName["gpu0"]
	if !ok {
		t.Fatal("gpu0 missing")
	}
	if igpu.PCIVendor != "8086" || igpu.PCIDevice != "64a0" {
		t.Errorf("gpu0 ids = %s:%s, want 8086:64a0", igpu.PCIVendor, igpu.PCIDevice)
	}
	if igpu.Driver != "xe" {
		t.Errorf("gpu0 driver = %q, want xe", igpu.Driver)
	}
	if !igpu.UnifiedMemory() {
		t.Error("gpu0 should be unified memory (no VRAM file)")
	}
	if igpu.PCIAddr != "0000:00:02.0" || igpu.PCIeRoot != "pci0000:00" {
		t.Errorf("gpu0 pci = %s / %s, want 0000:00:02.0 / pci0000:00", igpu.PCIAddr, igpu.PCIeRoot)
	}

	dgpu, ok := byName["gpu1"]
	if !ok {
		t.Fatal("gpu1 missing")
	}
	if dgpu.UnifiedMemory() {
		t.Error("gpu1 has VRAM; should not be unified")
	}
	if dgpu.VRAMBytes != 25753026560 {
		t.Errorf("gpu1 vram = %d, want 25753026560", dgpu.VRAMBytes)
	}

	npu, ok := byName["npu0"]
	if !ok {
		t.Fatal("npu0 missing")
	}
	if npu.Driver != "intel_vpu" {
		t.Errorf("npu0 driver = %q, want intel_vpu", npu.Driver)
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
