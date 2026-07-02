// Package probe walks the local device tree (sysfs + procfs) and produces a
// normalized inventory of accelerators. It is the "probe" half of llmfit's
// probe ⋈ index model: identity comes from here, capability comes from the
// index package.
package probe

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// Kind classifies a detected device.
type Kind string

const (
	KindGPU Kind = "gpu"
	KindNPU Kind = "npu"
	KindCPU Kind = "cpu"
)

// Device is one normalized entry from the device tree walk.
type Device struct {
	Kind      Kind
	Index     int
	PCIVendor string // e.g. "8086" (no 0x prefix)
	PCIDevice string // e.g. "64a0"
	PCIAddr   string // e.g. "0000:00:02.0"
	PCIeRoot  string // e.g. "pci0000:00"
	Driver    string // kernel driver, e.g. "xe", "intel_vpu"

	// VRAMBytes is dedicated device memory. 0 means none detected —
	// the device shares system RAM (unified memory).
	VRAMBytes uint64

	// DevNode is the primary /dev path, named by devtmpfs convention from
	// the sysfs class entry: /dev/dri/cardN for GPUs, /dev/accel/accelN
	// for NPUs; empty for the CPU device. RenderNode is the GPU's
	// unprivileged DRM render node (/dev/dri/renderD128), paired via the
	// device's sysfs drm/ dir. These are what the kubelet plugin injects
	// into containers via CDI (Phase 2); paths are host-relative and not
	// stat'ed — existence is the plugin's concern at prepare time.
	DevNode    string
	RenderNode string

	// CPUModel and SystemRAMBytes are set on the CPU fallback device.
	CPUModel       string
	SystemRAMBytes uint64

	// RASUncorrectable is the device's uncorrectable memory error count
	// (amdgpu RAS ue_count; 0 where the driver doesn't expose it).
	RASUncorrectable uint64
}

// Healthy reports whether the device is usable, with a machine-readable
// reason when it isn't. Facts only: a device with no bound kernel driver
// cannot be prepared, and uncorrectable ECC errors mean its memory lies.
// Event-driven health (XID/DCGM watches) is Phase 3 roadmap; this is the
// per-probe-cycle baseline.
func (d Device) Healthy() (bool, string) {
	if d.Kind == KindCPU {
		return true, ""
	}
	if d.Driver == "" {
		return false, "driverUnbound"
	}
	if d.RASUncorrectable > 0 {
		return false, "uncorrectableEcc"
	}
	return true, ""
}

// Name returns the stable DNS-label device name. PCI devices derive it from
// the PCI address — "gpu-0000-c3-00-0" — because DRA allocations and the
// kubelet plugin's prepare join on device NAMES: a name must identify the
// same silicon across reboots, hot-remove, and driver reloads, which an
// enumeration-order counter (gpu0, gpu1, …) does not. Non-PCI devices (the
// CPU fallback) keep the counter form; cpu0 is a stable singleton.
func (d Device) Name() string {
	if d.PCIAddr != "" {
		return string(d.Kind) + "-" + strings.NewReplacer(":", "-", ".", "-").Replace(d.PCIAddr)
	}
	return fmt.Sprintf("%s%d", d.Kind, d.Index)
}

// UnifiedMemory reports whether the device shares system RAM.
func (d Device) UnifiedMemory() bool {
	return d.Kind != KindCPU && d.VRAMBytes == 0
}

// Prober walks a sysfs/procfs tree. Roots are parameterized for tests and for
// containers that mount the host tree somewhere other than /.
type Prober struct {
	SysRoot  string // usually "/sys", or "/host/sys" in a container
	ProcRoot string // usually "/proc"
}

func New(sysRoot, procRoot string) *Prober {
	if sysRoot == "" {
		sysRoot = "/sys"
	}
	if procRoot == "" {
		procRoot = "/proc"
	}
	return &Prober{SysRoot: sysRoot, ProcRoot: procRoot}
}

// Walk enumerates GPUs (/sys/class/drm), NPUs (/sys/class/accel) and the CPU
// fallback device. The result is sorted by kind then index so consecutive
// walks of unchanged hardware compare deep-equal.
func (p *Prober) Walk() ([]Device, error) {
	var devices []Device

	gpus, err := p.walkClass(filepath.Join(p.SysRoot, "class", "drm"), "card", KindGPU)
	if err != nil {
		return nil, fmt.Errorf("walking drm class: %w", err)
	}
	devices = append(devices, gpus...)

	npus, err := p.walkClass(filepath.Join(p.SysRoot, "class", "accel"), "accel", KindNPU)
	if err != nil {
		return nil, fmt.Errorf("walking accel class: %w", err)
	}
	devices = append(devices, npus...)

	cpu, err := p.cpuDevice()
	if err != nil {
		return nil, fmt.Errorf("probing cpu: %w", err)
	}
	devices = append(devices, cpu)

	sort.Slice(devices, func(i, j int) bool {
		if devices[i].Kind != devices[j].Kind {
			return devices[i].Kind < devices[j].Kind
		}
		return devices[i].Index < devices[j].Index
	})
	return devices, nil
}

// walkClass enumerates entries like card0, accel0 under a /sys/class dir.
// Connector entries (card0-eDP-1) and render nodes are skipped by requiring
// the suffix after the prefix to be purely numeric.
func (p *Prober) walkClass(classDir, prefix string, kind Kind) ([]Device, error) {
	entries, err := os.ReadDir(classDir)
	if os.IsNotExist(err) {
		return nil, nil // class absent on this kernel — not an error
	}
	if err != nil {
		return nil, err
	}

	var out []Device
	idx := 0
	for _, e := range entries {
		suffix, ok := strings.CutPrefix(e.Name(), prefix)
		if !ok {
			continue
		}
		if _, err := strconv.Atoi(suffix); err != nil {
			continue
		}
		devDir := filepath.Join(classDir, e.Name(), "device")
		d := Device{
			Kind:      kind,
			Index:     idx,
			PCIVendor: readHexID(filepath.Join(devDir, "vendor")),
			PCIDevice: readHexID(filepath.Join(devDir, "device")),
			Driver:    readLinkBase(filepath.Join(devDir, "driver")),
			VRAMBytes: readUint(filepath.Join(devDir, "mem_info_vram_total")),

			RASUncorrectable: readUint(filepath.Join(devDir, "ras", "ue_count")),
		}
		d.PCIAddr, d.PCIeRoot = pciAddress(devDir)
		switch kind {
		case KindGPU:
			d.DevNode = "/dev/dri/" + e.Name()
			d.RenderNode = renderNode(devDir)
		case KindNPU:
			d.DevNode = "/dev/accel/" + e.Name()
		}
		out = append(out, d)
		idx++
	}
	return out, nil
}

func (p *Prober) cpuDevice() (Device, error) {
	ram, err := memTotalBytes(filepath.Join(p.ProcRoot, "meminfo"))
	if err != nil {
		return Device{}, err
	}
	return Device{
		Kind:           KindCPU,
		Index:          0,
		CPUModel:       cpuModel(filepath.Join(p.ProcRoot, "cpuinfo")),
		SystemRAMBytes: ram,
	}, nil
}

// MemTotalBytes exposes system RAM for callers that need it to size
// unified-memory devices.
func (p *Prober) MemTotalBytes() (uint64, error) {
	return memTotalBytes(filepath.Join(p.ProcRoot, "meminfo"))
}

// renderNode pairs a DRM card with its render node. The device dir's drm/
// subdir lists every DRM minor backed by that PCI device (cardN plus
// renderDNNN), so the pairing survives multi-GPU enumeration where render
// minors (128, 129, …) don't line up with card indices.
func renderNode(devDir string) string {
	entries, err := os.ReadDir(filepath.Join(devDir, "drm"))
	if err != nil {
		return "" // no render node (e.g. driver without render capability)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "renderD") {
			return "/dev/dri/" + e.Name()
		}
	}
	return ""
}

func readHexID(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimPrefix(strings.TrimSpace(string(b)), "0x")
}

func readLinkBase(path string) string {
	target, err := os.Readlink(path)
	if err != nil {
		return ""
	}
	return filepath.Base(target)
}

func readUint(path string) uint64 {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	v, err := strconv.ParseUint(strings.TrimSpace(string(b)), 10, 64)
	if err != nil {
		return 0
	}
	return v
}

// pciAddress resolves the device dir to its PCI address and root complex,
// e.g. ("0000:00:02.0", "pci0000:00").
func pciAddress(devDir string) (addr, root string) {
	resolved, err := filepath.EvalSymlinks(devDir)
	if err != nil {
		return "", ""
	}
	for _, part := range strings.Split(resolved, string(filepath.Separator)) {
		if strings.HasPrefix(part, "pci") && strings.Contains(part, ":") {
			root = part
		}
		// PCI addresses look like 0000:00:02.0
		if len(part) == 12 && part[4] == ':' && part[7] == ':' && part[10] == '.' {
			addr = part
		}
	}
	return addr, root
}

func memTotalBytes(meminfoPath string) (uint64, error) {
	f, err := os.Open(meminfoPath)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) >= 2 && fields[0] == "MemTotal:" {
			kb, err := strconv.ParseUint(fields[1], 10, 64)
			if err != nil {
				return 0, err
			}
			return kb * 1024, nil
		}
	}
	return 0, fmt.Errorf("MemTotal not found in %s", meminfoPath)
}

func cpuModel(cpuinfoPath string) string {
	f, err := os.Open(cpuinfoPath)
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "model name") {
			if _, val, ok := strings.Cut(line, ":"); ok {
				return strings.TrimSpace(val)
			}
		}
	}
	return ""
}
