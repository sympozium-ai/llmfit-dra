// Package publisher converts probed+indexed devices into DRA ResourceSlices
// and keeps them synchronized via the upstream resourceslice helper
// controller. DeviceClasses ship in deploy/deviceclass.yaml, so claims
// allocate against these devices; the kubelet plugin (NodePrepareResources →
// CDI) is the remaining Phase 2 piece before pods can run against claims.
package publisher

import (
	"context"
	"errors"
	"fmt"
	"unicode/utf8"

	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/client-go/kubernetes"
	"k8s.io/dynamic-resource-allocation/resourceslice"
	"k8s.io/klog/v2"
	"k8s.io/utils/ptr"

	"github.com/sympozium-ai/llmfit-dra/internal/index"
	"github.com/sympozium-ai/llmfit-dra/internal/llmfit"
	"github.com/sympozium-ai/llmfit-dra/internal/observe"
	"github.com/sympozium-ai/llmfit-dra/pkg/probe"
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
	// Probed GPU counts gate the llmfit⋈probe pairing: one llmfit entry may
	// only serve several probed cards when they are identical models
	// (entry.Count covers them). Counting up front keeps matchLLMFitGPU pure.
	probedGPUs := 0
	probedGPUsByVendor := map[string]int{}
	for _, d := range devices {
		if d.Kind == probe.KindGPU {
			probedGPUs++
			probedGPUsByVendor[vendorName(d.PCIVendor)]++
		}
	}

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
		case probe.KindNIC:
			// Fabric endpoint: identity + link facts, no capability join —
			// llmfit's model DB and the PCI-ID index price accelerators, not
			// HCAs. No memory capacity is published, so model-fit CEL never
			// selects a NIC (same mechanism that excludes virtual display
			// adapters). The value is the companion pattern: a claim's second
			// request aligned with a GPU on resource.kubernetes.io/pcieRoot.
			attrs["vendor"] = resourceapi.DeviceAttribute{StringValue: ptr.To(vendorName(d.PCIVendor))}
			attrs["source"] = resourceapi.DeviceAttribute{StringValue: ptr.To("probe")}
			if d.Driver != "" {
				attrs["driver"] = resourceapi.DeviceAttribute{StringValue: ptr.To(d.Driver)}
			}
			if d.PCIAddr != "" {
				attrs["pciAddress"] = resourceapi.DeviceAttribute{StringValue: ptr.To(d.PCIAddr)}
			}
			if d.PCIeRoot != "" {
				attrs["pcieRoot"] = resourceapi.DeviceAttribute{StringValue: ptr.To(d.PCIeRoot)}
				attrs["resource.kubernetes.io/pcieRoot"] = resourceapi.DeviceAttribute{StringValue: ptr.To(d.PCIeRoot)}
			}
			if d.IBLinkLayer != "" {
				// "infiniband" or "ethernet" (RoCE) — the plane, per the
				// fabric design: backplane speed and RDMA speed are not the
				// same network.
				attrs["linkLayer"] = resourceapi.DeviceAttribute{StringValue: ptr.To(d.IBLinkLayer)}
			}
			if d.IBRateGbps > 0 {
				attrs["rateGbps"] = resourceapi.DeviceAttribute{IntValue: ptr.To(int64(d.IBRateGbps))}
			}
			if d.NetDev != "" {
				attrs["netdev"] = resourceapi.DeviceAttribute{StringValue: ptr.To(d.NetDev)}
			}
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
			lf := matchLLMFitGPU(sys, d, vendor, probedGPUsByVendor[vendor], probedGPUs)
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
				// Compute is index-only for now: llmfit publishes no FLOPS
				// signal, so the curated number rides along even when llmfit
				// wins the capability join.
				if found && entry.ComputeTFLOPS > 0 {
					attrs["computeTFLOPS"] = resourceapi.DeviceAttribute{IntValue: ptr.To(entry.ComputeTFLOPS)}
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
				if entry.ComputeTFLOPS > 0 {
					attrs["computeTFLOPS"] = resourceapi.DeviceAttribute{IntValue: ptr.To(entry.ComputeTFLOPS)}
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
				case unified != nil && *unified:
					// Unified pool first: on APUs the sysfs VRAM file is the
					// BIOS UMA carve-out (often <4Gi of a >64Gi pool), so a
					// llmfit outage must not collapse a Strix Halo's capacity
					// to the carve-out. Take whichever is larger.
					memBytes = max(d.VRAMBytes, systemRAM)
				case d.VRAMBytes > 0:
					memBytes = d.VRAMBytes
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
// same-vendor entry legitimately serves several probed cards — but only as
// many as the entry's count covers. When the probe sees MORE same-vendor
// cards than llmfit reported (llmfit under-reports: ROCm sees the supported
// dGPU but not the iGPU next to it), vendor alone cannot say which probed
// card the entry describes. Two or more same-vendor entries are ambiguous
// for the same reason. Both cases return nil: the per-PCI-ID index gives
// each card its own correct numbers instead. (Per-instance pairing needs
// llmfit to report PCI identity — tracked upstream.)
func matchLLMFitGPU(sys *llmfit.System, d probe.Device, vendor string, sameVendorProbed, totalProbed int) *llmfit.GPU {
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
		covered := int(match.Count)
		if covered == 0 {
			covered = 1
		}
		if sameVendorProbed > covered {
			return nil // llmfit under-reported this vendor: pairing unknowable
		}
		return match
	}
	// Last resort — one GPU on BOTH sides and an llmfit name too generic to
	// classify: only then trust the pairing. A classifiable-but-different
	// vendor is a real mismatch (llmfit priced a card the probe attributes
	// to another vendor — e.g. a BMC framebuffer next to the real GPU) and
	// must never inherit its capability.
	if len(sys.GPUs) == 1 && totalProbed == 1 && llmfit.VendorOf(sys.GPUs[0]) == "" {
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
// The ErrorHandler surfaces write failures the helper would otherwise keep to
// its own logs — API rejections and DroppedFieldsError (fields like taints
// silently dropped by a server without the feature gate).
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
		ErrorHandler: func(ctx context.Context, err error, msg string) {
			var dropped *resourceslice.DroppedFieldsError
			kind := "api"
			if errors.As(err, &dropped) {
				kind = "dropped_fields"
			}
			observe.SlicePublishError(kind)
			klog.FromContext(ctx).Error(err, "resourceslice publish: "+msg, "kind", kind)
		},
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
	case "15b3":
		return "mellanox"
	case "14e4":
		return "broadcom"
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

// truncate caps s at n bytes without splitting a multi-byte rune — an
// invalid-UTF-8 attribute value would make the API server reject the whole
// ResourceSlice write.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	s = s[:n]
	for len(s) > 0 && !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s
}
