package index

import (
	"crypto/sha256"
	_ "embed"
	"encoding/json"
	"fmt"
	"sort"
)

//go:embed nvidia_boards.json
var rawNvidiaBoards []byte

// NvidiaBoard describes one NVIDIA board the way the NVIDIA DRA driver names
// it. Keyed by productName rather than PCI ID because MIG devices share the
// parent's PCI identity and gpu.nvidia.com ResourceSlices carry no PCI
// attributes on MIG devices — productName is the only stable join key.
type NvidiaBoard struct {
	// ProductName is the exact gpu.nvidia.com/productName attribute value.
	ProductName string `json:"-"`
	// MemoryMiB is nominal board memory. Published MIG capacities run ~2-5%
	// under the nominal slice size (reserved VRAM), which is why thresholds
	// are placed at slice midpoints, never at slice boundaries.
	MemoryMiB int64 `json:"memoryMiB"`
	// MemoryBandwidthGBs is peak board bandwidth — the number the derived
	// per-slice bandwidth model scales down from.
	MemoryBandwidthGBs int64 `json:"memoryBandwidthGBs"`
	// ComputeTFLOPS is effective dense FP16 throughput. 0 = unknown; the
	// board is then excluded whenever a compute floor is set.
	ComputeTFLOPS int64 `json:"computeTFLOPS,omitempty"`
	// SMCount is the full-board multiprocessor count, matching the
	// gpu.nvidia.com multiprocessors capacity on the 7g/full profile.
	SMCount int64 `json:"smCount,omitempty"`
	// MemorySlices is the MIG memory-slice count (8 for A100/H100/H200,
	// 4 for A30). 0 = the board does not support MIG.
	MemorySlices int64 `json:"memorySlices,omitempty"`
}

// NvidiaBoards is the curated NVIDIA board table — same probe ⋈ index split
// as the PCI index: identity (productName) arrives in the vendor's
// ResourceSlices; capability (bandwidth, slice geometry) cannot, so it ships
// as data.
type NvidiaBoards struct {
	boards  map[string]NvidiaBoard
	version string
}

// LoadNvidiaBoards parses the embedded board table. The version string is a
// content hash, so template annotations force a re-render whenever the table
// ships different data.
func LoadNvidiaBoards() (*NvidiaBoards, error) {
	var boards map[string]NvidiaBoard
	if err := json.Unmarshal(rawNvidiaBoards, &boards); err != nil {
		return nil, fmt.Errorf("parsing embedded nvidia board table: %w", err)
	}
	for name, b := range boards {
		b.ProductName = name
		boards[name] = b
	}
	sum := sha256.Sum256(rawNvidiaBoards)
	return &NvidiaBoards{boards: boards, version: fmt.Sprintf("%x", sum[:4])}, nil
}

// ByProductName returns the board for an exact productName attribute value.
// found=false means the board fails closed: no fit CEL admits it, and the
// satisfiability diagnostics name it as unknown.
func (n *NvidiaBoards) ByProductName(name string) (NvidiaBoard, bool) {
	b, ok := n.boards[name]
	return b, ok
}

// All returns every board sorted by productName so generated CEL is
// deterministic across runs.
func (n *NvidiaBoards) All() []NvidiaBoard {
	out := make([]NvidiaBoard, 0, len(n.boards))
	for _, b := range n.boards {
		out = append(out, b)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ProductName < out[j].ProductName })
	return out
}

// Version is the content hash of the embedded table (annotation drift key).
func (n *NvidiaBoards) Version() string { return n.version }

// Len reports the number of known boards (used by tests and startup logs).
func (n *NvidiaBoards) Len() int { return len(n.boards) }
