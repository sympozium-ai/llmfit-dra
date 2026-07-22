package modelclaim

import (
	"fmt"
	"sort"

	resourceapi "k8s.io/api/resource/v1"

	"github.com/sympozium-ai/llmfit-dra/internal/index"
)

// EvaluateNvidiaSlices statically mirrors NvidiaFitCEL over published
// gpu.nvidia.com ResourceSlices — the same advisory computation
// EvaluateSlices does for our own slices, in the NVIDIA driver's vocabulary.
// No healthy gate: the NVIDIA driver publishes no health attribute, and the
// CEL therefore tests none. Bandwidth numbers in messages are always labeled
// "derived" — they come from llmfit's slice-fraction model, not from
// anything NVIDIA publishes.
func EvaluateNvidiaSlices(slices []*resourceapi.ResourceSlice, allocated map[string]string, b *Bounds, deviceClassName string, minComputeTFLOPS int64, boards *index.NvidiaBoards) Candidates {
	wantMIG, wantGPU := nvidiaBranchesFor(deviceClassName)
	memFloorMiB := nvidiaMemFloorMiB(b)
	maxGen := currentPoolGens(slices, NvidiaDriverDomain)

	nodes := map[string]bool{}
	devices := 0
	available := 0
	heldBy := ""
	bestMsg := ""
	bestScore := -1.0 // higher = closer to satisfying

	// Boards missing from the table fail closed in the CEL, so they must not
	// be silent here: an operator whose whole fleet is unknown needs "update
	// the board table", not "no devices".
	unknownBoards := map[string]bool{}

	for _, slice := range slices {
		if slice.Spec.Driver != NvidiaDriverDomain {
			continue
		}
		if slice.Spec.Pool.Generation < maxGen[slice.Spec.Pool.Name] {
			continue // superseded by a newer generation of the same pool
		}
		node := ""
		if slice.Spec.NodeName != nil {
			node = *slice.Spec.NodeName
		}
		for i := range slice.Spec.Devices {
			dev := &slice.Spec.Devices[i]
			attrs := dev.Attributes

			devType := ""
			if a, ok := attrs["type"]; ok && a.StringValue != nil {
				devType = *a.StringValue
			}
			isMIG := devType == "mig"
			if (isMIG && !wantMIG) || (devType == "gpu" && !wantGPU) || (devType != "mig" && devType != "gpu") {
				continue
			}

			product := ""
			if a, ok := attrs["productName"]; ok && a.StringValue != nil {
				product = *a.StringValue
			}
			var memMiB int64
			if cap, ok := dev.Capacity["memory"]; ok {
				memMiB = cap.Value.Value() / (1024 * 1024)
			}

			board, known := boards.ByProductName(product)
			if !known {
				unknownBoards[product] = true
				continue // fails closed, matching the CEL
			}

			// Mirror the CEL's admission for this device.
			var thrMiB, minSMs int64
			feasible := true
			if isMIG {
				var ok bool
				if thrMiB, ok = migThresholdMiB(board, b); !ok {
					feasible = false
				}
				if minSMs, ok = migMinSMs(board, minComputeTFLOPS); !ok {
					feasible = false
				}
			} else {
				thrMiB = memFloorMiB
				feasible = gpuAdmits(board, b, minComputeTFLOPS)
			}

			memOK := feasible && memMiB >= thrMiB
			smOK := true
			if isMIG && minSMs > 0 {
				var sms int64
				if cap, ok := dev.Capacity["multiprocessors"]; ok {
					sms = cap.Value.Value()
				}
				smOK = sms >= minSMs
			}

			if memOK && smOK {
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

			// Nearest miss for the shortfall message, scored by derived
			// bandwidth ratio — the axis the operator can actually act on
			// (repartition to a bigger profile).
			derivedBW := int64(0)
			if board.MemoryMiB > 0 {
				derivedBW = board.MemoryBandwidthGBs * memMiB / board.MemoryMiB
			}
			score := 0.0
			if b.MinBandwidthGBs > 0 && derivedBW > 0 {
				score = float64(derivedBW) / float64(b.MinBandwidthGBs)
			}
			if score > bestScore {
				bestScore = score
				label := dev.Name
				if isMIG {
					if p, ok := attrs["profile"]; ok && p.StringValue != nil {
						label = fmt.Sprintf("%s profile %s", dev.Name, *p.StringValue)
					}
				}
				var reasons []string
				if !feasible {
					reasons = append(reasons, fmt.Sprintf("no partition of %s can meet the bounds (board peak %d GB/s, %d slices — llmfit derived model)",
						product, board.MemoryBandwidthGBs, board.MemorySlices))
				} else {
					if memMiB < thrMiB {
						if isMIG {
							reasons = append(reasons, fmt.Sprintf("memory %dMi < %dMi threshold (derived bandwidth %d < %d GB/s at board peak %d GB/s)",
								memMiB, thrMiB, derivedBW, b.MinBandwidthGBs, board.MemoryBandwidthGBs))
						} else {
							reasons = append(reasons, fmt.Sprintf("memory %dMi < %dMi", memMiB, thrMiB))
						}
					}
					if !smOK {
						reasons = append(reasons, fmt.Sprintf("multiprocessors below %d (compute floor %d TFLOPS)", minSMs, minComputeTFLOPS))
					}
				}
				sort.Strings(reasons)
				bestMsg = fmt.Sprintf("closest NVIDIA device %s (%s, node %s): %s",
					label, product, node, join(reasons))
			}
		}
	}

	c := Candidates{Devices: devices, Nodes: len(nodes), Available: available, HeldBy: heldBy}
	if devices == 0 {
		if bestMsg == "" {
			bestMsg = "no gpu.nvidia.com devices published — is the NVIDIA DRA driver installed?"
		}
		if len(unknownBoards) > 0 {
			names := make([]string, 0, len(unknownBoards))
			for n := range unknownBoards {
				names = append(names, fmt.Sprintf("%q", n))
			}
			sort.Strings(names)
			bestMsg = fmt.Sprintf("unknown NVIDIA board(s) %s not in llmfit board table %s — devices fail closed; %s",
				join(names), boards.Version(), bestMsg)
		}
		c.Shortfall = bestMsg
	}
	return c
}
