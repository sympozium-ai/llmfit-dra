// Package publisher converts probed+indexed devices into DRA ResourceSlices
// and keeps them synchronized via the upstream resourceslice helper
// controller. Publish-only (Phase 1): there is no kubelet plugin and no
// DeviceClass, so devices are visible to schedulers and controllers but not
// yet claimable end-to-end.
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

// BuildDevices maps probed devices into DRA device entries. Capability is
// sourced, in order of preference: llmfit (real assessment layer, nil-able),
// the embedded index, bare probe facts. The probe always supplies identity
// (PCI address, root, driver). systemRAM sizes unified-memory devices.
func BuildDevices(devices []probe.Device, idx *index.Index, systemRAM uint64, sys *llmfit.System) []resourceapi.Device {
	out := make([]resourceapi.Device, 0, len(devices))
	for _, d := range devices {
		attrs := map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
			"kind":    {StringValue: ptr.To(string(d.Kind))},
			"healthy": {BoolValue: ptr.To(true)},
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
			}

			// Capability, best source first: llmfit → index → probe.
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
				if lf.MemoryBandwidthGBps != nil && *lf.MemoryBandwidthGBps > 0 {
					attrs["memoryBandwidthGBs"] = resourceapi.DeviceAttribute{IntValue: ptr.To(int64(*lf.MemoryBandwidthGBps))}
				}
				attrs["unifiedMemory"] = resourceapi.DeviceAttribute{BoolValue: ptr.To(lf.UnifiedMemory)}
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
				attrs["unifiedMemory"] = resourceapi.DeviceAttribute{BoolValue: ptr.To(d.UnifiedMemory() || entry.UnifiedMemory)}
			default:
				attrs["source"] = resourceapi.DeviceAttribute{StringValue: ptr.To("probe")}
				attrs["model"] = resourceapi.DeviceAttribute{StringValue: ptr.To(truncate(fmt.Sprintf("pci-%s-%s", d.PCIVendor, d.PCIDevice), 64))}
				attrs["unifiedMemory"] = resourceapi.DeviceAttribute{BoolValue: ptr.To(d.UnifiedMemory())}
			}

			// Probe-detected VRAM carve-outs win when llmfit had no number.
			if memBytes == 0 {
				if d.VRAMBytes > 0 {
					memBytes = d.VRAMBytes
				} else {
					memBytes = systemRAM
				}
			}
		}

		dev := resourceapi.Device{
			Name:       d.Name(),
			Attributes: attrs,
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
// llmfit groups identical models (count > 1), so one llmfit entry may serve
// several probed cards of the same vendor.
func matchLLMFitGPU(sys *llmfit.System, d probe.Device, vendor string) *llmfit.GPU {
	if sys == nil || d.Kind != probe.KindGPU {
		return nil
	}
	for i := range sys.GPUs {
		if llmfit.VendorOf(sys.GPUs[i]) == vendor {
			return &sys.GPUs[i]
		}
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
