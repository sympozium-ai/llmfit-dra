// Package index is the curated accelerator capability index — the half of
// llmfit's probe ⋈ index model that the OS cannot provide. Identity (PCI IDs)
// is discoverable; capability (memory bandwidth, marketing name, unified
// memory semantics) is not, so it ships as data.
package index

import (
	_ "embed"
	"encoding/json"
	"fmt"
)

//go:embed data.json
var rawData []byte

// Entry describes what we know about one accelerator model.
type Entry struct {
	// Model is the human/marketing name, e.g. "Intel Arc Graphics 140V".
	Model string `json:"model"`
	// MemoryBandwidthGBs is peak memory bandwidth in GB/s — the number the
	// token-generation (decode) physics hangs off. 0 = unknown.
	MemoryBandwidthGBs int64 `json:"memoryBandwidthGBs"`
	// ComputeTFLOPS is effective dense FP16 throughput in TFLOPS (tensor-core
	// where applicable, no sparsity) — the number prefill/TTFT physics hangs
	// off, from vendor spec sheets. 0 = unknown; the attribute is then not
	// published, and compute-floored claims will not match the device.
	ComputeTFLOPS int64 `json:"computeTFLOPS,omitempty"`
	// UnifiedMemory marks devices that share system RAM by design.
	UnifiedMemory bool `json:"unifiedMemory,omitempty"`
}

// Index maps "vendor:device" PCI ID pairs (lowercase hex, no 0x) to entries.
type Index struct {
	entries map[string]Entry
}

// Load parses the embedded capability data.
func Load() (*Index, error) {
	var entries map[string]Entry
	if err := json.Unmarshal(rawData, &entries); err != nil {
		return nil, fmt.Errorf("parsing embedded index: %w", err)
	}
	return &Index{entries: entries}, nil
}

// Lookup returns the entry for a PCI vendor/device pair and whether it was
// found. Callers publish found=false as indexed=false so consumers can
// distinguish "unknown hardware" from "known slow hardware".
func (i *Index) Lookup(pciVendor, pciDevice string) (Entry, bool) {
	e, ok := i.entries[pciVendor+":"+pciDevice]
	return e, ok
}

// Len reports the number of known accelerators (used by tests and startup logs).
func (i *Index) Len() int { return len(i.entries) }
