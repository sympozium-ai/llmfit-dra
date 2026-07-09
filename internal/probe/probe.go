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

	"k8s.io/klog/v2"
)

// Kind classifies a detected device.
type Kind string

const (
	KindGPU Kind = "gpu"
	KindNPU Kind = "npu"
	KindCPU Kind = "cpu"
	// KindNIC is an RDMA-capable fabric endpoint (/sys/class/infiniband).
	// NICs are inventory for the fabric axis — "a GPU with the right NIC
	// beside it" — never compute: a PCI function that appears under the
	// infiniband class is excluded from the GPU/NPU walks (DPUs and
	// converged cards present both faces).
	KindNIC Kind = "nic"
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

	// VRAMBytes is dedicated device memory as read from driver-specific
	// sysfs (see vramBytes). 0 means none DETECTED — true for unified
	// iGPUs but also for drivers that expose no VRAM file (NVIDIA
	// proprietary), so 0 must not be read as "unified".
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

	// UniqueID is amdgpu's stable GPU identifier (sysfs unique_id) where the
	// ASIC exposes one; the kubelet plugin uses it for ROCR_VISIBLE_DEVICES
	// isolation on multi-GPU AMD nodes. Empty on APUs/ASICs without it.
	UniqueID string

	// NIC-only fields, read from the device's first port (multi-port HCAs
	// are published as one device describing port 1 — a per-port split is
	// deferred until a real dual-fabric node needs it).
	//
	// IBLinkLayer is the normalized transport: "infiniband" or "ethernet"
	// (RoCE). IBRateGbps is the active link rate. IBPortActive is the port
	// state — a down port is inventory, not capacity, and publishes
	// unhealthy. NetDev is the associated netdev name (RoCE; empty on
	// IB-only ports).
	IBLinkLayer  string
	IBRateGbps   uint64
	IBPortActive bool
	NetDev       string
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
	if d.Kind == KindNIC {
		if !d.IBPortActive {
			return false, "portDown"
		}
		if d.DevNode == "" {
			// No uverbs char device (ib_uverbs not loaded): the kubelet
			// plugin has nothing to inject, so the NIC must not be
			// allocatable through the healthy-gated default class.
			return false, "noUverbsNode"
		}
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

// Walk enumerates GPUs (/sys/class/drm), NPUs (/sys/class/accel), RDMA NICs
// (/sys/class/infiniband) and the CPU fallback device. The result is sorted
// by kind then index so consecutive walks of unchanged hardware compare
// deep-equal.
//
// Classification rule: a PCI function that appears under the infiniband
// class is a fabric endpoint, not compute — it is dropped from the GPU/NPU
// results even when it also registers a drm/accel entry (BlueField-class
// DPUs, converged cards, and vendor offload engines present both faces).
func (p *Prober) Walk() ([]Device, error) {
	var devices []Device

	nics, err := p.walkInfiniband()
	if err != nil {
		return nil, fmt.Errorf("walking infiniband class: %w", err)
	}
	fabricPCI := map[string]bool{}
	for _, n := range nics {
		if n.PCIAddr != "" {
			fabricPCI[n.PCIAddr] = true
		}
	}

	gpus, err := p.walkClass(filepath.Join(p.SysRoot, "class", "drm"), "card", KindGPU)
	if err != nil {
		return nil, fmt.Errorf("walking drm class: %w", err)
	}
	devices = append(devices, dropFabricFunctions(gpus, fabricPCI)...)

	npus, err := p.walkClass(filepath.Join(p.SysRoot, "class", "accel"), "accel", KindNPU)
	if err != nil {
		return nil, fmt.Errorf("walking accel class: %w", err)
	}
	devices = append(devices, dropFabricFunctions(npus, fabricPCI)...)

	devices = append(devices, nics...)

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
			VRAMBytes: vramBytes(classDir, e.Name()),
			UniqueID:  readTrimmed(filepath.Join(devDir, "unique_id")),

			RASUncorrectable: readUint(filepath.Join(devDir, "ras", "ue_count")),
		}
		d.PCIAddr, d.PCIeRoot = pciAddress(devDir)
		switch kind {
		case KindGPU:
			d.DevNode = "/dev/dri/" + e.Name()
			d.RenderNode = renderNode(devDir)
			if d.VRAMBytes == 0 {
				// "0 VRAM" is deliberately ambiguous (unified iGPU vs a
				// driver with no sysfs VRAM file) — say so per device, or a
				// capacity-less published GPU is undebuggable from logs.
				klog.V(2).InfoS("no VRAM sysfs path readable; unknown here (unified iGPU or driver without sysfs VRAM)",
					"device", e.Name(), "driver", d.Driver)
			}
		case KindNPU:
			d.DevNode = "/dev/accel/" + e.Name()
		}
		out = append(out, d)
		idx++
	}
	return out, nil
}

// dropFabricFunctions filters compute candidates whose PCI function is also
// an infiniband HCA — the classification rule from Walk's contract.
func dropFabricFunctions(devices []Device, fabricPCI map[string]bool) []Device {
	out := devices[:0]
	for _, d := range devices {
		if d.PCIAddr != "" && fabricPCI[d.PCIAddr] {
			klog.V(1).InfoS("PCI function is an infiniband HCA; classified as fabric endpoint, not compute",
				"pciAddress", d.PCIAddr, "droppedKind", d.Kind)
			continue
		}
		out = append(out, d)
	}
	return out
}

// walkInfiniband enumerates RDMA-capable devices under /sys/class/infiniband.
// Identity comes from the PCI device dir like every other kind; NIC facts
// come from the first port (link_layer, rate, state) and the device's net/
// subdir (the paired netdev on RoCE). The injectable /dev node is the
// device's uverbs char device, paired via /sys/class/infiniband_verbs.
func (p *Prober) walkInfiniband() ([]Device, error) {
	classDir := filepath.Join(p.SysRoot, "class", "infiniband")
	entries, err := os.ReadDir(classDir)
	if os.IsNotExist(err) {
		return nil, nil // no RDMA stack on this kernel — not an error
	}
	if err != nil {
		return nil, err
	}

	var out []Device
	idx := 0
	for _, e := range entries {
		devDir := filepath.Join(classDir, e.Name(), "device")
		if _, err := os.Stat(devDir); err != nil {
			continue // not a device-backed entry
		}
		port1 := filepath.Join(classDir, e.Name(), "ports", "1")
		d := Device{
			Kind:         KindNIC,
			Index:        idx,
			PCIVendor:    readHexID(filepath.Join(devDir, "vendor")),
			PCIDevice:    readHexID(filepath.Join(devDir, "device")),
			Driver:       readLinkBase(filepath.Join(devDir, "driver")),
			IBLinkLayer:  normalizeLinkLayer(readTrimmed(filepath.Join(port1, "link_layer"))),
			IBRateGbps:   parseRateGbps(readTrimmed(filepath.Join(port1, "rate"))),
			IBPortActive: strings.Contains(readTrimmed(filepath.Join(port1, "state")), "ACTIVE"),
			NetDev:       firstDirEntry(filepath.Join(devDir, "net")),
			DevNode:      uverbsNode(p.SysRoot, e.Name()),
		}
		d.PCIAddr, d.PCIeRoot = pciAddress(devDir)
		out = append(out, d)
		idx++
	}
	return out, nil
}

// normalizeLinkLayer maps sysfs port link_layer values ("InfiniBand",
// "Ethernet") to stable lowercase attribute values.
func normalizeLinkLayer(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}

// parseRateGbps parses a sysfs port rate like "100 Gb/sec (4X EDR)" — the
// leading number is the active link rate. Fractional rates (SDR 2.5 Gb/sec)
// round down; 0 means unreadable.
func parseRateGbps(s string) uint64 {
	fields := strings.Fields(s)
	if len(fields) == 0 {
		return 0
	}
	v, err := strconv.ParseFloat(fields[0], 64)
	if err != nil || v < 0 {
		return 0
	}
	return uint64(v)
}

// firstDirEntry returns the sole (or first, sorted) entry of a directory —
// used for the HCA's net/ subdir, which lists its paired netdev.
func firstDirEntry(dir string) string {
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) == 0 {
		return ""
	}
	return entries[0].Name()
}

// uverbsNode pairs an ibdev with its user-verbs char device: each
// /sys/class/infiniband_verbs/uverbsN names its owner in an ibdev file, and
// the /dev/infiniband/uverbsN node follows devtmpfs convention. Empty when
// the verbs class is absent (kernel without ib_uverbs) — the device is then
// inventory the plugin cannot prepare.
func uverbsNode(sysRoot, ibdev string) string {
	verbsDir := filepath.Join(sysRoot, "class", "infiniband_verbs")
	entries, err := os.ReadDir(verbsDir)
	if err != nil {
		return ""
	}
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), "uverbs") {
			continue
		}
		if readTrimmed(filepath.Join(verbsDir, e.Name(), "ibdev")) == ibdev {
			return "/dev/infiniband/" + e.Name()
		}
	}
	return ""
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

// vramBytes reads dedicated device memory from the driver-specific sysfs
// location: amdgpu's mem_info_vram_total (device dir), i915's
// lmem_total_bytes (card dir, discrete only), or xe's per-tile
// physical_vram_size_bytes. 0 means no dedicated VRAM was DETECTED — which
// is what unified-memory iGPUs and NVIDIA's proprietary driver (no sysfs
// VRAM file at all) both look like, so 0 must never be interpreted as
// "unified"; only as "unknown here".
func vramBytes(classDir, entry string) uint64 {
	devDir := filepath.Join(classDir, entry, "device")
	for _, p := range []string{
		filepath.Join(devDir, "mem_info_vram_total"),               // amdgpu
		filepath.Join(classDir, entry, "lmem_total_bytes"),         // i915 dGPU
		filepath.Join(devDir, "tile0", "physical_vram_size_bytes"), // xe dGPU
	} {
		if v := readUint(p); v > 0 {
			return v
		}
	}
	return 0
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

func readTrimmed(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
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
