package llmfit

import "testing"

// Captured from `llmfit --json system` on a Strix Halo Framework desktop
// (fields trimmed to what we parse).
const strixJSON = `{
  "system": {
    "available_ram_gb": 48.2,
    "backend": "Vulkan",
    "cpu_cores": 32,
    "cpu_name": "AMD RYZEN AI MAX+ 395 w/ Radeon 8060S",
    "gpu_count": 1,
    "gpu_name": "AMD Radeon 8060S (Strix Halo)",
    "gpus": [
      {
        "backend": "Vulkan",
        "count": 1,
        "memory_bandwidth_gbps": 256.0,
        "name": "AMD Radeon 8060S (Strix Halo)",
        "unified_memory": true,
        "vram_gb": 62.56
      }
    ],
    "has_gpu": true,
    "total_ram_gb": 62.56,
    "unified_memory": true
  }
}`

func TestParse(t *testing.T) {
	sys, err := Parse([]byte(strixJSON))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if sys.CPUName != "AMD RYZEN AI MAX+ 395 w/ Radeon 8060S" {
		t.Errorf("cpu_name = %q", sys.CPUName)
	}
	if len(sys.GPUs) != 1 {
		t.Fatalf("expected 1 gpu, got %d", len(sys.GPUs))
	}
	g := sys.GPUs[0]
	if g.MemoryBandwidthGBps == nil || *g.MemoryBandwidthGBps != 256.0 {
		t.Errorf("bandwidth = %v, want 256", g.MemoryBandwidthGBps)
	}
	if !g.UnifiedMemory {
		t.Error("expected unified memory")
	}
	if g.Backend != "Vulkan" {
		t.Errorf("backend = %q", g.Backend)
	}
}

func TestParseNullBandwidth(t *testing.T) {
	sys, err := Parse([]byte(`{"system":{"cpu_name":"x","gpus":[{"name":"Intel Arc","backend":"SYCL","count":1,"memory_bandwidth_gbps":null,"vram_gb":0.0}]}}`))
	if err != nil {
		t.Fatal(err)
	}
	if sys.GPUs[0].MemoryBandwidthGBps != nil {
		t.Error("null bandwidth should parse to nil")
	}
}

func TestParseRejectsEmpty(t *testing.T) {
	if _, err := Parse([]byte(`{"system":{}}`)); err == nil {
		t.Error("empty system should be rejected")
	}
}

func TestVendorOf(t *testing.T) {
	cases := map[string]GPU{
		"amd":    {Name: "AMD Radeon 8060S (Strix Halo)", Backend: "Vulkan"},
		"nvidia": {Name: "NVIDIA GeForce RTX 4090", Backend: "CUDA"},
		"intel":  {Name: "Intel Arc", Backend: "SYCL"},
		"":       {Name: "Mystery Device", Backend: "OpenCL"},
	}
	for want, gpu := range cases {
		if got := VendorOf(gpu); got != want {
			t.Errorf("VendorOf(%q) = %q, want %q", gpu.Name, got, want)
		}
	}
}
