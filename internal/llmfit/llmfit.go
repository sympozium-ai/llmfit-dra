// Package llmfit shells out to the real llmfit binary (Rust) for hardware
// capability assessment. llmfit owns detection nuance the generic probe
// can't know — APU unified-memory pools, vendor-tool fallbacks (nvidia-smi,
// rocm-smi, lspci), and its curated memory-bandwidth database. The probe
// package still supplies PCI identity; llmfit supplies capability.
package llmfit

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// GPU is one entry from `llmfit --json system` .system.gpus[].
type GPU struct {
	Name                string   `json:"name"`
	VRAMGB              *float64 `json:"vram_gb"`
	Backend             string   `json:"backend"`
	Count               uint32   `json:"count"`
	UnifiedMemory       bool     `json:"unified_memory"`
	MemoryBandwidthGBps *float64 `json:"memory_bandwidth_gbps"`
}

// System mirrors the parts of llmfit's SystemSpecs JSON we publish.
type System struct {
	TotalRAMGB    float64 `json:"total_ram_gb"`
	CPUCores      int     `json:"cpu_cores"`
	CPUName       string  `json:"cpu_name"`
	HasGPU        bool    `json:"has_gpu"`
	UnifiedMemory bool    `json:"unified_memory"`
	Backend       string  `json:"backend"`
	GPUs          []GPU   `json:"gpus"`
}

type output struct {
	System System `json:"system"`
}

// Detect runs `<bin> --json system` and parses the result.
func Detect(ctx context.Context, bin string) (*System, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	out, err := exec.CommandContext(ctx, bin, "--json", "system").Output()
	if err != nil {
		var ee *exec.ExitError
		if e, ok := err.(*exec.ExitError); ok {
			ee = e
		}
		detail := ""
		if ee != nil {
			detail = ": " + strings.TrimSpace(string(ee.Stderr))
		}
		return nil, fmt.Errorf("running %q --json system: %w%s", bin, err, detail)
	}
	return Parse(out)
}

// Parse decodes llmfit's system JSON.
func Parse(data []byte) (*System, error) {
	var o output
	if err := json.Unmarshal(data, &o); err != nil {
		return nil, fmt.Errorf("parsing llmfit system JSON: %w", err)
	}
	if o.System.CPUName == "" && len(o.System.GPUs) == 0 {
		return nil, fmt.Errorf("llmfit system JSON contained no hardware")
	}
	return &o.System, nil
}

// VendorOf guesses the PCI-style vendor for an llmfit GPU name, used to
// correlate llmfit entries with probed PCI devices.
func VendorOf(g GPU) string {
	n := strings.ToLower(g.Name + " " + g.Backend)
	switch {
	case strings.Contains(n, "nvidia") || strings.Contains(n, "geforce") || strings.Contains(n, "cuda"):
		return "nvidia"
	case strings.Contains(n, "amd") || strings.Contains(n, "radeon") || strings.Contains(n, "rocm") || strings.Contains(n, "instinct"):
		return "amd"
	case strings.Contains(n, "intel") || strings.Contains(n, "arc") || strings.Contains(n, "sycl"):
		return "intel"
	case strings.Contains(n, "apple") || strings.Contains(n, "metal"):
		return "apple"
	default:
		return ""
	}
}
