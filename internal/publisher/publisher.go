// Package publisher converts probed+indexed devices into DRA ResourceSlices
// and keeps them synchronized via the upstream resourceslice helper
// controller. DeviceClasses ship in deploy/deviceclass.yaml, so claims
// allocate against these devices; the kubelet plugin (NodePrepareResources →
// CDI) is the remaining Phase 2 piece before pods can run against claims.
package publisher

import (
	"context"
	"fmt"

	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/client-go/kubernetes"
	"k8s.io/dynamic-resource-allocation/resourceslice"
	"k8s.io/utils/ptr"

	"github.com/sympozium-ai/llmfit-dra/internal/index"
	"github.com/sympozium-ai/llmfit-dra/internal/llmfit"
	"github.com/sympozium-ai/llmfit-dra/internal/probe"
)

// DriverName is the DRA driver identity. Attribute names without a domain
// prefix are implicitly scoped to this driver.
const DriverName = "llmfit.ai"

// preparableGPUDrivers are the kernel drivers whose GPUs the kubelet plugin
// can actually make usable — the open stacks where DRM render-node access is
// the whole runtime (amdgpu also gets /dev/kfd). A GPU on any other driver,
// notably NVIDIA's proprietary "nvidia" (which needs /dev/nvidia* and vendor
// libraries the plugin does not inject), is published fitness-only. Keep in
// sync with nodeplugin.editsFor.
var preparableGPUDrivers = map[string]bool{
	"amdgpu": true,
	"xe":     true,
	"i915":   true,
}

// Options tunes BuildDevices beyond its inputs.
type Options struct {
	// Taints adds a NoSchedule device taint to unhealthy devices. Behind
	// an option because DRADeviceTaints is an alpha feature gate: servers
	// without it silently drop the field, which the resourceslice helper
	// reports as a DroppedFieldsError on every sync.
	Taints bool

	// VendorManagedGPUs marks every GPU with the vendorManaged attribute:
	// a vendor DRA driver owns allocation of this node's GPUs, and the
	// shipped DeviceClasses exclude vendorManaged devices so the same
	// silicon is never allocatable through two drivers. Attributes stay
	// published — the fitness/companion matchAttribute pattern remains
	// available through a custom class that opts in.
	VendorManagedGPUs bool
}

// BuildDevices maps probed devices into DRA device entries. Capability is
// sourced, in order of preference: llmfit (real assessment layer, nil-able),
// the embedded index, bare probe facts. The probe always supplies identity
// (PCI address, root, driver). systemRAM sizes unified-memory devices.
func BuildDevices(devices []probe.Device, idx *index.Index, systemRAM uint64, sys *llmfit.System, opts Options) []resourceapi.Device {
	out := make([]resourceapi.Device, 0, len(devices))
	for _, d := range devices {
		healthy, reason := d.Healthy()
		attrs := map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
			"kind":    {StringValue: ptr.To(string(d.Kind))},
			"healthy": {BoolValue: ptr.To(healthy)},
		}
		if reason != "" {
			attrs["healthReason"] = resourceapi.DeviceAttribute{StringValue: ptr.To(reason)}
		}

		var memBytes uint64
		switch d.Kind {
		case probe.KindCPU:
			attrs["vendor"] = resourceapi.DeviceAttribute{StringValue: ptr.To("cpu")}
			attrs["unifiedMemory"] = resourceapi.DeviceAttribute{BoolValue: ptr.To(true)}
			model := d.CPUModel
			memBytes = d.SystemRAMBytes
			if sys != nil {
				attrs["source"] = resourceapi.DeviceAttribute{StringValue: ptr.To("llmfit")}
				if sys.CPUName != "" {
					model = sys.CPUName
				}
				if sys.TotalRAMGB > 0 {
					memBytes = uint64(sys.TotalRAMGB * 1024 * 1024 * 1024)
				}
			} else {
				attrs["source"] = resourceapi.DeviceAttribute{StringValue: ptr.To("probe")}
			}
			if model != "" {
				attrs["model"] = resourceapi.DeviceAttribute{StringValue: ptr.To(truncate(model, 64))}
			}
		default:
			vendor := vendorName(d.PCIVendor)
			attrs["vendor"] = resourceapi.DeviceAttribute{StringValue: ptr.To(vendor)}
			if d.Driver != "" {
				attrs["driver"] = resourceapi.DeviceAttribute{StringValue: ptr.To(d.Driver)}
			}
			if d.PCIAddr != "" {
				attrs["pciAddress"] = resourceapi.DeviceAttribute{StringValue: ptr.To(d.PCIAddr)}
			}
			if d.PCIeRoot != "" {
				attrs["pcieRoot"] = resourceapi.DeviceAttribute{StringValue: ptr.To(d.PCIeRoot)}
				// The standardized cross-driver spelling (Phase 3): lets a
				// single claim align our device with a vendor driver's via
				// constraints.matchAttribute, since both drivers publish the
				// same qualified attribute for the same silicon.
				attrs["resource.kubernetes.io/pcieRoot"] = resourceapi.DeviceAttribute{StringValue: ptr.To(d.PCIeRoot)}
			}

			// Capability, best source first: llmfit → index → probe.
			// unified stays nil when no source KNOWS: "the probe found no
			// VRAM file" is what NVIDIA's proprietary driver looks like too,
			// so it must not imply unified memory.
			var unified *bool
			lf := matchLLMFitGPU(sys, d, vendor)
			entry, found := idx.Lookup(d.PCIVendor, d.PCIDevice)
			attrs["indexed"] = resourceapi.DeviceAttribute{BoolValue: ptr.To(found)}
			switch {
			case lf != nil:
				attrs["source"] = resourceapi.DeviceAttribute{StringValue: ptr.To("llmfit")}
				attrs["model"] = resourceapi.DeviceAttribute{StringValue: ptr.To(truncate(lf.Name, 64))}
				if lf.Backend != "" {
					attrs["backend"] = resourceapi.DeviceAttribute{StringValue: ptr.To(lf.Backend)}
				}
				switch {
				case lf.MemoryBandwidthGBps != nil && *lf.MemoryBandwidthGBps > 0:
					attrs["memoryBandwidthGBs"] = resourceapi.DeviceAttribute{IntValue: ptr.To(int64(*lf.MemoryBandwidthGBps))}
				case found && entry.MemoryBandwidthGBs > 0:
					// llmfit matched the device but couldn't price its
					// bandwidth (e.g. a stale pci.ids gave lspci a generic
					// name). The PCI-ID index still can.
					attrs["memoryBandwidthGBs"] = resourceapi.DeviceAttribute{IntValue: ptr.To(entry.MemoryBandwidthGBs)}
				}
				unified = ptr.To(lf.UnifiedMemory)
				// llmfit's vram_gb is the fit budget (for APUs it already
				// resolves to the shared pool).
				if lf.VRAMGB != nil && *lf.VRAMGB > 0 {
					memBytes = uint64(*lf.VRAMGB * 1024 * 1024 * 1024)
				}
			case found:
				attrs["source"] = resourceapi.DeviceAttribute{StringValue: ptr.To("index")}
				attrs["model"] = resourceapi.DeviceAttribute{StringValue: ptr.To(truncate(entry.Model, 64))}
				if entry.MemoryBandwidthGBs > 0 {
					attrs["memoryBandwidthGBs"] = resourceapi.DeviceAttribute{IntValue: ptr.To(entry.MemoryBandwidthGBs)}
				}
				unified = ptr.To(entry.UnifiedMemory)
			default:
				attrs["source"] = resourceapi.DeviceAttribute{StringValue: ptr.To("probe")}
				attrs["model"] = resourceapi.DeviceAttribute{StringValue: ptr.To(truncate(fmt.Sprintf("pci-%s-%s", d.PCIVendor, d.PCIDevice), 64))}
				if d.VRAMBytes > 0 {
					unified = ptr.To(false) // dedicated VRAM measured: definitely discrete
				}
			}
			if unified != nil {
				attrs["unifiedMemory"] = resourceapi.DeviceAttribute{BoolValue: unified}
			}
			// Fitness-only (vendorManaged, excluded from the default classes)
			// when EITHER a vendor DRA driver owns this node's GPUs, OR the
			// kernel driver is one the kubelet plugin cannot prepare — a
			// default-class claim must never allocate a device we can't make
			// usable. NVIDIA's proprietary driver is the motivating case: we
			// inject the DRM render node, but CUDA needs /dev/nvidia* and
			// vendor libraries we do not, so a bare NVIDIA node (no vendor DRA
			// driver installed) would otherwise ship a fit-selectable but
			// unpreparable GPU. The attributes stay published for the
			// fitness-companion matchAttribute pattern.
			if d.Kind == probe.KindGPU && (opts.VendorManagedGPUs || !preparableGPUDrivers[d.Driver]) {
				attrs["vendorManaged"] = resourceapi.DeviceAttribute{BoolValue: ptr.To(true)}
			}

			// Capacity is only published when we actually know it: a probed
			// VRAM carve-out, or the shared pool on a KNOWN unified device.
			// An unknown discrete card (NVIDIA proprietary, unprobed Intel
			// dGPU) gets NO capacity — advertising system RAM as VRAM places
			// models onto cards that cannot hold them.
			if memBytes == 0 {
				switch {
				case d.VRAMBytes > 0:
					memBytes = d.VRAMBytes
				case unified != nil && *unified:
					memBytes = systemRAM
				}
			}
		}

		dev := resourceapi.Device{
			Name:       d.Name(),
			Attributes: attrs,
		}
		if opts.Taints && !healthy {
			dev.Taints = []resourceapi.DeviceTaint{{
				Key:    DriverName + "/unhealthy",
				Value:  reason,
				Effect: resourceapi.DeviceTaintEffectNoSchedule,
			}}
		}
		if memBytes > 0 {
			dev.Capacity = map[resourceapi.QualifiedName]resourceapi.DeviceCapacity{
				"memory": {Value: *resource.NewQuantity(int64(memBytes), resource.BinarySI)},
			}
		}
		out = append(out, dev)
	}
	return out
}

// matchLLMFitGPU pairs a probed GPU with an llmfit-reported GPU by vendor.
// llmfit groups IDENTICAL models into one entry (count > 1), so a single
// same-vendor entry legitimately serves several probed cards. Two or more
// same-vendor entries mean different models — vendor alone cannot say which
// probed card is which, and guessing applies the first card's bandwidth and
// memory to all of them. Ambiguity returns nil: the per-PCI-ID index gives
// each card its own correct numbers instead. (Per-instance pairing needs
// llmfit to report PCI identity — tracked upstream.)
func matchLLMFitGPU(sys *llmfit.System, d probe.Device, vendor string) *llmfit.GPU {
	if sys == nil || d.Kind != probe.KindGPU {
		return nil
	}
	var match *llmfit.GPU
	for i := range sys.GPUs {
		if llmfit.VendorOf(sys.GPUs[i]) == vendor {
			if match != nil {
				return nil // ambiguous: multiple distinct same-vendor models
			}
			match = &sys.GPUs[i]
		}
	}
	if match != nil {
		return match
	}
	// Single GPU on both sides: trust the pairing even if the vendor
	// heuristic can't classify the llmfit name.
	if len(sys.GPUs) == 1 {
		return &sys.GPUs[0]
	}
	return nil
}

// BuildResources wraps devices into the helper controller's desired-state
// type: one pool named after the node, one slice.
func BuildResources(nodeName string, devices []resourceapi.Device) *resourceslice.DriverResources {
	return &resourceslice.DriverResources{
		Pools: map[string]resourceslice.Pool{
			nodeName: {
				Slices: []resourceslice.Slice{{Devices: devices}},
			},
		},
	}
}

// Start launches the resourceslice helper controller with node ownership, so
// slices are garbage-collected with the node and identified via spec.nodeName.
func Start(ctx context.Context, client kubernetes.Interface, nodeName string, resources *resourceslice.DriverResources) (*resourceslice.Controller, error) {
	return resourceslice.StartController(ctx, resourceslice.Options{
		DriverName: DriverName,
		KubeClient: client,
		Owner: &resourceslice.Owner{
			APIVersion: "v1",
			Kind:       "Node",
			Name:       nodeName,
		},
		Resources: resources,
	})
}

func vendorName(pciVendor string) string {
	switch pciVendor {
	case "8086":
		return "intel"
	case "10de":
		return "nvidia"
	case "1002", "1022":
		return "amd"
	case "1da3":
		return "habana"
	// Virtual display adapters (hypervisor framebuffers). Published as
	// facts like everything else — they carry no bandwidth attribute, so
	// fit CEL never selects them.
	case "1414":
		return "microsoft"
	case "15ad":
		return "vmware"
	case "1af4":
		return "virtio"
	default:
		return "pci-" + pciVendor
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
