package index

import "testing"

func TestLoad(t *testing.T) {
	idx, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if idx.Len() == 0 {
		t.Fatal("embedded index is empty")
	}
}

func TestLookupKnown(t *testing.T) {
	idx, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	e, ok := idx.Lookup("8086", "64a0")
	if !ok {
		t.Fatal("8086:64a0 (Arc 140V) should be indexed")
	}
	if e.Model != "Intel Arc Graphics 140V" {
		t.Errorf("model = %q", e.Model)
	}
	if e.MemoryBandwidthGBs != 136 {
		t.Errorf("bandwidth = %d, want 136", e.MemoryBandwidthGBs)
	}
	if !e.UnifiedMemory {
		t.Error("Arc 140V is an iGPU; unifiedMemory should be true")
	}
}

func TestLookupUnknown(t *testing.T) {
	idx, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := idx.Lookup("dead", "beef"); ok {
		t.Error("unknown PCI pair should not resolve")
	}
}

func TestAllEntriesHaveModelNames(t *testing.T) {
	idx, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	for key, e := range idx.entries {
		if e.Model == "" {
			t.Errorf("entry %s has no model name", key)
		}
		if len(e.Model) > 64 {
			t.Errorf("entry %s model exceeds the 64-char DRA attribute limit", key)
		}
	}
}
