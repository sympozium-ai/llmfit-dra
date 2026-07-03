package modelclaim

import (
	"fmt"
	"sort"

	resourceapi "k8s.io/api/resource/v1"
)

// Candidates is the satisfiability snapshot: how many published devices
// (across how many nodes) meet the resolved bounds right now.
type Candidates struct {
	Devices int
	Nodes   int
	// Available is the subset of Devices not currently held by an allocated
	// ResourceClaim. Physics-satisfiable vs available-right-now is exactly
	// the distinction an operator needs when a pod is Pending: Devices == 0
	// means "never (as published)"; Available == 0 means "queue or scale"
	// (issue #21).
	Available int
	// HeldBy names an example holder (namespace/name) when matching devices
	// exist but none are available.
	HeldBy string
	// Shortfall explains the nearest miss when Devices == 0 — the answer to
	// "why would my pod be Pending" before any pod exists.
	Shortfall string
}

// AllocatedDevices maps "pool/device" keys held by allocated ResourceClaims
// for this driver to their holder ("namespace/name").
func AllocatedDevices(claims []*resourceapi.ResourceClaim) map[string]string {
	held := map[string]string{}
	for _, rc := range claims {
		if rc.Status.Allocation == nil {
			continue
		}
		for _, r := range rc.Status.Allocation.Devices.Results {
			if r.Driver != DriverDomain {
				continue
			}
			held[r.Pool+"/"+r.Device] = rc.Namespace + "/" + rc.Name
		}
	}
	return held
}

// kindForClass mirrors the shipped DeviceClass selectors: the kind classes
// pin device kind; the base class allows any non-vendorManaged device.
func kindForClass(deviceClassName string) string {
	switch deviceClassName {
	case "gpu." + DriverDomain:
		return "gpu"
	case "cpu." + DriverDomain:
		return "cpu"
	case "npu." + DriverDomain:
		return "npu"
	default:
		return "" // any kind
	}
}

// EvaluateSlices statically checks the resolved bounds against published
// ResourceSlices. The generated constraint is a known inequality — memory,
// bandwidth, healthy — so no CEL engine is needed; this stays a cheap,
// advisory computation (it never gates template creation).
func EvaluateSlices(slices []*resourceapi.ResourceSlice, allocated map[string]string, b *Bounds, deviceClassName string) Candidates {
	wantKind := kindForClass(deviceClassName)
	memFloor := int64(b.MemoryGi) * 1024 * 1024 * 1024

	nodes := map[string]bool{}
	devices := 0
	available := 0
	heldBy := ""
	bestMsg := ""
	bestScore := -1.0 // higher = closer to satisfying

	for _, slice := range slices {
		if slice.Spec.Driver != DriverDomain {
			continue
		}
		node := ""
		if slice.Spec.NodeName != nil {
			node = *slice.Spec.NodeName
		}
		for i := range slice.Spec.Devices {
			dev := &slice.Spec.Devices[i]
			attrs := dev.Attributes

			if wantKind != "" {
				if k := attrs["kind"]; k.StringValue == nil || *k.StringValue != wantKind {
					continue
				}
			}
			if deviceClassName == DriverDomain || deviceClassName == "gpu."+DriverDomain {
				if vm := attrs["vendorManaged"]; vm.BoolValue != nil && *vm.BoolValue {
					continue // demoted: the default classes exclude it
				}
			}

			var memOK, bwOK, healthyOK bool
			var mem int64
			if cap, ok := dev.Capacity["memory"]; ok {
				mem = cap.Value.Value()
				memOK = mem >= memFloor
			}
			var bw int64
			if a, ok := attrs["memoryBandwidthGBs"]; ok && a.IntValue != nil {
				bw = *a.IntValue
				bwOK = bw >= int64(b.MinBandwidthGBs)
			}
			if h, ok := attrs["healthy"]; ok && h.BoolValue != nil {
				healthyOK = *h.BoolValue
			}

			if memOK && bwOK && healthyOK {
				devices++
				if node != "" {
					nodes[node] = true
				}
				key := slice.Spec.Pool.Name + "/" + dev.Name
				if holder, held := allocated[key]; held {
					if heldBy == "" {
						heldBy = holder
					}
				} else {
					available++
				}
				continue
			}

			// Track the nearest miss for the shortfall message. Score by the
			// fraction of constraints met, tie-broken by bandwidth ratio.
			score := 0.0
			for _, ok := range []bool{memOK, bwOK, healthyOK} {
				if ok {
					score += 1.0
				}
			}
			if b.MinBandwidthGBs > 0 && bw > 0 {
				score += float64(bw) / float64(b.MinBandwidthGBs) * 0.1
			}
			if score > bestScore {
				bestScore = score
				var reasons []string
				if !memOK {
					reasons = append(reasons, fmt.Sprintf("memory %dGi < %dGi", mem/(1024*1024*1024), b.MemoryGi))
				}
				if !bwOK {
					if bw == 0 {
						reasons = append(reasons, "no memoryBandwidthGBs published")
					} else {
						reasons = append(reasons, fmt.Sprintf("bandwidth %d < %d GB/s", bw, b.MinBandwidthGBs))
					}
				}
				if !healthyOK {
					reasons = append(reasons, "unhealthy")
				}
				sort.Strings(reasons)
				bestMsg = fmt.Sprintf("closest device %s (node %s): %s",
					dev.Name, node, join(reasons))
			}
		}
	}

	c := Candidates{Devices: devices, Nodes: len(nodes), Available: available, HeldBy: heldBy}
	if devices == 0 {
		if bestMsg == "" {
			bestMsg = "no llmfit.ai devices published"
		}
		c.Shortfall = bestMsg
	}
	return c
}

func join(parts []string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += ", "
		}
		out += p
	}
	return out
}
