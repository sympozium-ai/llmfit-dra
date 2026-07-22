package modelclaim

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/sympozium-ai/llmfit-dra/internal/index"
)

// The NVIDIA translation backend compiles the same resolved physics FitCEL
// encodes into the NVIDIA DRA driver's vocabulary. gpu.nvidia.com publishes
// no bandwidth attribute, so the bandwidth floor cannot be tested directly —
// but derived MIG bandwidth is proportional to the device's published memory
// capacity (each memory slice carries dedicated capacity AND bandwidth, per
// the MIG User Guide), so both floors collapse into one per-board capacity
// threshold. That is llmfit's DERIVED model, not an NVIDIA-published
// per-profile spec — diagnostics say "derived" for exactly this reason.
//
// Thresholds sit at the midpoint between the last-rejected and
// first-admitted memory-slice count: published MIG capacities run ~2-5%
// under nominal slice size (reserved VRAM), and adjacent slice sizes differ
// by ≥1/slices (12.5% on 8-slice boards), so midpoints are robust where
// exact slice boundaries are not.
//
// Boards absent from the table fail closed: no clause admits them, and the
// satisfiability diagnostics name them as unknown.

// nvidiaMemFloorMiB is the memory floor in MiB. Bounds carries Gi, but MIG
// thresholds need sub-Gi precision.
func nvidiaMemFloorMiB(b *Bounds) int64 { return int64(b.MemoryGi) * 1024 }

// migThresholdMiB collapses the memory and bandwidth floors into one
// capacity threshold for MIG partitions of the board. ok=false means no MIG
// partition of this board can ever satisfy the bounds (not MIG-capable,
// unknown bandwidth, floor above board capacity, or bandwidth beyond the
// full board).
func migThresholdMiB(board index.NvidiaBoard, b *Bounds) (int64, bool) {
	if board.MemorySlices <= 0 || board.MemoryBandwidthGBs <= 0 {
		return 0, false
	}
	memFloor := nvidiaMemFloorMiB(b)
	thr := memFloor
	if bw := int64(b.MinBandwidthGBs); bw > 0 {
		// Minimum admissible memory-slice count, then the midpoint between
		// k-1 and k slices of board memory.
		k := (bw*board.MemorySlices + board.MemoryBandwidthGBs - 1) / board.MemoryBandwidthGBs
		if k > board.MemorySlices {
			return 0, false
		}
		if k >= 1 {
			if mid := (2*k - 1) * board.MemoryMiB / (2 * board.MemorySlices); mid > thr {
				thr = mid
			}
		}
	}
	if thr > board.MemoryMiB {
		return 0, false
	}
	return thr, true
}

// migMinSMs converts an opt-in compute floor into a multiprocessors-capacity
// threshold via the board's SM fraction. ok=false excludes the board: a
// compute floor against a board with no compute data is a contradiction the
// diagnostics should say out loud, mirroring FitCEL's never-waived clause.
func migMinSMs(board index.NvidiaBoard, minComputeTFLOPS int64) (int64, bool) {
	if minComputeTFLOPS <= 0 {
		return 0, true
	}
	if board.ComputeTFLOPS <= 0 || board.SMCount <= 0 {
		return 0, false
	}
	minSMs := (minComputeTFLOPS*board.SMCount + board.ComputeTFLOPS - 1) / board.ComputeTFLOPS
	if minSMs > board.SMCount {
		return 0, false
	}
	return minSMs, true
}

// gpuAdmits reports whether a FULL board satisfies the bounds — the whole
// device carries the whole board's bandwidth and compute, so admission is a
// board-level check and the emitted clause only re-tests memory.
func gpuAdmits(board index.NvidiaBoard, b *Bounds, minComputeTFLOPS int64) bool {
	if board.MemoryBandwidthGBs < int64(b.MinBandwidthGBs) {
		return false
	}
	if board.MemoryMiB < nvidiaMemFloorMiB(b) {
		return false
	}
	if minComputeTFLOPS > 0 && board.ComputeTFLOPS < minComputeTFLOPS {
		return false
	}
	return true
}

// migGroup is one emitted MIG clause: every board sharing a threshold pair
// collapses into a single productName list to keep CEL cost down.
type migGroup struct {
	thresholdMiB int64
	minSMs       int64
	products     []string
}

// migGroups computes the admissible (threshold, minSMs) groups, sorted for
// deterministic CEL output.
func migGroups(boards *index.NvidiaBoards, b *Bounds, minComputeTFLOPS int64) []migGroup {
	byKey := map[string]*migGroup{}
	for _, board := range boards.All() {
		thr, ok := migThresholdMiB(board, b)
		if !ok {
			continue
		}
		minSMs, ok := migMinSMs(board, minComputeTFLOPS)
		if !ok {
			continue
		}
		key := strconv.FormatInt(thr, 10) + "/" + strconv.FormatInt(minSMs, 10)
		g, ok := byKey[key]
		if !ok {
			g = &migGroup{thresholdMiB: thr, minSMs: minSMs}
			byKey[key] = g
		}
		g.products = append(g.products, board.ProductName)
	}
	groups := make([]migGroup, 0, len(byKey))
	for _, g := range byKey {
		sort.Strings(g.products)
		groups = append(groups, *g)
	}
	sort.Slice(groups, func(i, j int) bool {
		if groups[i].thresholdMiB != groups[j].thresholdMiB {
			return groups[i].thresholdMiB < groups[j].thresholdMiB
		}
		return groups[i].minSMs < groups[j].minSMs
	})
	return groups
}

// celProductList renders a sorted CEL string-list literal.
func celProductList(products []string) string {
	quoted := make([]string, len(products))
	for i, p := range products {
		quoted[i] = "'" + p + "'"
	}
	return "[" + strings.Join(quoted, ", ") + "]"
}

// NvidiaFitCEL renders the fit inequality against gpu.nvidia.com device
// attributes. Same membership-guard discipline as FitCEL: a missing
// attribute means "no match", never a CEL runtime error. An empty admissible
// set compiles to 'false' — structurally unsatisfiable, which the
// Satisfiable condition then explains.
func NvidiaFitCEL(b *Bounds, boards *index.NvidiaBoards, deviceClassName string, minComputeTFLOPS int64) string {
	n := NvidiaDriverDomain
	wantMIG, wantGPU := nvidiaBranchesFor(deviceClassName)

	var branches []string
	if wantMIG {
		if groups := migGroups(boards, b, minComputeTFLOPS); len(groups) > 0 {
			var clauses []string
			for _, g := range groups {
				clause := fmt.Sprintf(
					"(device.attributes['%[1]s'].productName in %[2]s && "+
						"device.capacity['%[1]s'].memory.compareTo(quantity('%[3]dMi')) >= 0",
					n, celProductList(g.products), g.thresholdMiB)
				if g.minSMs > 0 {
					clause += fmt.Sprintf(" && "+
						"'multiprocessors' in device.capacity['%[1]s'] && "+
						"device.capacity['%[1]s'].multiprocessors.compareTo(quantity('%[2]d')) >= 0",
						n, g.minSMs)
				}
				clauses = append(clauses, clause+")")
			}
			branches = append(branches, fmt.Sprintf(
				"(device.attributes['%[1]s'].type == 'mig' && (%[2]s))",
				n, strings.Join(clauses, " || ")))
		}
	}
	if wantGPU {
		var admitted []string
		for _, board := range boards.All() {
			if gpuAdmits(board, b, minComputeTFLOPS) {
				admitted = append(admitted, board.ProductName)
			}
		}
		if len(admitted) > 0 {
			sort.Strings(admitted)
			branches = append(branches, fmt.Sprintf(
				"(device.attributes['%[1]s'].type == 'gpu' && "+
					"device.attributes['%[1]s'].productName in %[2]s && "+
					"device.capacity['%[1]s'].memory.compareTo(quantity('%[3]dMi')) >= 0)",
				n, celProductList(admitted), nvidiaMemFloorMiB(b)))
		}
	}
	if len(branches) == 0 {
		// Nothing in the board table can satisfy the bounds (or the table
		// does not know the fleet). Fail closed and let Satisfiable explain.
		return "false"
	}
	return fmt.Sprintf(
		"'type' in device.attributes['%[1]s'] && "+
			"'productName' in device.attributes['%[1]s'] && "+
			"'memory' in device.capacity['%[1]s'] && "+
			"(%[2]s)",
		n, strings.Join(branches, " || "))
}

// migProfilePattern parses NVIDIA MIG profile names, e.g. 1g.5gb, 3g.40gb,
// 1g.10gb+me. Diagnostics only — allocation math rides on published memory
// capacity, never on profile-string parsing, so unknown future profile
// shapes degrade to an unlabeled (but still correct) message.
var migProfilePattern = regexp.MustCompile(`^([0-9]+)g\.([0-9]+)gb(\+me)?$`)

// parseMIGProfile extracts the compute-slice count and nominal memory GB
// from a profile name.
func parseMIGProfile(s string) (g, gb int64, me bool, ok bool) {
	m := migProfilePattern.FindStringSubmatch(s)
	if m == nil {
		return 0, 0, false, false
	}
	g, _ = strconv.ParseInt(m[1], 10, 64)
	gb, _ = strconv.ParseInt(m[2], 10, 64)
	return g, gb, m[3] != "", true
}
