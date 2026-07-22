package index

import "testing"

func TestLoadNvidiaBoards(t *testing.T) {
	boards, err := LoadNvidiaBoards()
	if err != nil {
		t.Fatalf("LoadNvidiaBoards: %v", err)
	}
	if boards.Len() == 0 {
		t.Fatal("embedded board table is empty")
	}
	if boards.Version() == "" {
		t.Fatal("version hash must be non-empty (template annotations ride on it)")
	}

	// The join key is the exact productName; a lookup must round-trip it.
	b, ok := boards.ByProductName("NVIDIA A100-SXM4-80GB")
	if !ok {
		t.Fatal("A100-SXM4-80GB missing from board table")
	}
	if b.ProductName != "NVIDIA A100-SXM4-80GB" {
		t.Errorf("ProductName not populated from key: %q", b.ProductName)
	}
	if b.MemorySlices != 8 || b.MemoryBandwidthGBs != 2039 {
		t.Errorf("unexpected A100-SXM4-80GB data: %+v", b)
	}

	if _, ok := boards.ByProductName("NVIDIA Imaginary GPU"); ok {
		t.Error("unknown boards must fail closed (found=false)")
	}
}

func TestNvidiaBoardsInvariants(t *testing.T) {
	boards, err := LoadNvidiaBoards()
	if err != nil {
		t.Fatalf("LoadNvidiaBoards: %v", err)
	}
	all := boards.All()
	for i, b := range all {
		if i > 0 && all[i-1].ProductName >= b.ProductName {
			t.Fatalf("All() must sort by productName for deterministic CEL: %q before %q",
				all[i-1].ProductName, b.ProductName)
		}
		if b.MemoryMiB <= 0 || b.MemoryBandwidthGBs <= 0 {
			t.Errorf("%s: memory and bandwidth are load-bearing, must be positive: %+v", b.ProductName, b)
		}
		// A30 is the one seeded 4-slice board; every other MIG board is 8.
		if b.MemorySlices != 0 && b.MemorySlices != 8 && b.MemorySlices != 4 {
			t.Errorf("%s: unexpected memorySlices %d", b.ProductName, b.MemorySlices)
		}
	}
}
