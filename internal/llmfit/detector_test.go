package llmfit

import (
	"context"
	"errors"
	"net"
	"net/http"
	"path/filepath"
	"testing"
	"time"
)

func sys(name string) *System { return &System{CPUName: name} }

func TestDetectorPrefersAPIOverExec(t *testing.T) {
	d := &Detector{
		api:      func(context.Context) (*System, error) { return sys("via-api"), nil },
		exec:     func(context.Context) (*System, error) { return sys("via-exec"), nil },
		maxStale: time.Minute,
	}
	got, err := d.Detect(context.Background())
	if err != nil || got.CPUName != "via-api" {
		t.Fatalf("got %v, %v", got, err)
	}
}

func TestDetectorFallsBackToExec(t *testing.T) {
	d := &Detector{
		api:      func(context.Context) (*System, error) { return nil, errors.New("socket down") },
		exec:     func(context.Context) (*System, error) { return sys("via-exec"), nil },
		maxStale: time.Minute,
	}
	got, err := d.Detect(context.Background())
	if err != nil || got.CPUName != "via-exec" {
		t.Fatalf("got %v, %v", got, err)
	}
}

func TestDetectorServesLastKnownGoodWithinBound(t *testing.T) {
	calls := 0
	d := &Detector{
		api: func(context.Context) (*System, error) {
			calls++
			if calls == 1 {
				return sys("fresh"), nil
			}
			return nil, errors.New("hiccup")
		},
		maxStale: time.Minute,
	}
	if _, err := d.Detect(context.Background()); err != nil {
		t.Fatal(err)
	}
	got, err := d.Detect(context.Background())
	if err != nil || got.CPUName != "fresh" {
		t.Fatalf("expected cached result during hiccup, got %v, %v", got, err)
	}
}

// The T4 flap from issue #38: the exec transport knows the GPU's bandwidth,
// then the API transport takes over with a response that omits the field
// (llmfit <1.1.3, AlexsJones/llmfit#747). The known value must carry
// forward instead of vanishing from published slices.
func TestDetectorBackfillsBandwidthAcrossTransports(t *testing.T) {
	bw := 320.0
	apiCalls := 0
	d := &Detector{
		api: func(context.Context) (*System, error) {
			apiCalls++
			if apiCalls == 1 {
				return nil, errors.New("sidecar not ready")
			}
			return &System{CPUName: "c", GPUs: []GPU{{Name: "Tesla T4"}}}, nil
		},
		exec: func(context.Context) (*System, error) {
			return &System{CPUName: "c", GPUs: []GPU{{Name: "Tesla T4", MemoryBandwidthGBps: &bw}}}, nil
		},
		maxStale: time.Minute,
	}
	if _, err := d.Detect(context.Background()); err != nil { // exec serves, bandwidth known
		t.Fatal(err)
	}
	got, err := d.Detect(context.Background()) // api takes over, field omitted
	if err != nil {
		t.Fatal(err)
	}
	if got.GPUs[0].MemoryBandwidthGBps == nil || *got.GPUs[0].MemoryBandwidthGBps != 320 {
		t.Fatalf("bandwidth must survive the exec→api handoff, got %+v", got.GPUs[0])
	}
}

func TestDetectorBackfillDoesNotClobberFreshValue(t *testing.T) {
	old, fresh := 100.0, 320.0
	d := &Detector{maxStale: time.Minute}
	d.last = &System{GPUs: []GPU{{Name: "G", MemoryBandwidthGBps: &old}}}
	d.lastAt = time.Now()
	d.api = func(context.Context) (*System, error) {
		return &System{CPUName: "c", GPUs: []GPU{{Name: "G", MemoryBandwidthGBps: &fresh}}}, nil
	}
	got, err := d.Detect(context.Background())
	if err != nil || *got.GPUs[0].MemoryBandwidthGBps != 320 {
		t.Fatalf("fresh value must win, got %+v, %v", got.GPUs, err)
	}
}

func TestDetectorRefusesStaleCache(t *testing.T) {
	d := &Detector{
		api:      func(context.Context) (*System, error) { return nil, errors.New("down") },
		maxStale: time.Minute,
	}
	d.last, d.lastAt = sys("ancient"), time.Now().Add(-2*time.Minute)
	if _, err := d.Detect(context.Background()); err == nil {
		t.Fatal("stale cache must not be served past maxStale")
	}
}

func TestClientOverUnixSocket(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "llmfit.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/system", func(w http.ResponseWriter, r *http.Request) {
		// The serve API envelope: node metadata plus the same system object
		// the CLI emits.
		w.Write([]byte(`{"node":{"name":"n1","os":"linux"},"system":{"cpu_name":"Test CPU","total_ram_gb":32,"has_gpu":false,"gpus":[]}}`))
	})
	go http.Serve(l, mux)

	c, err := NewClient("unix://" + sock)
	if err != nil {
		t.Fatal(err)
	}
	got, err := c.Detect(context.Background())
	if err != nil || got.CPUName != "Test CPU" {
		t.Fatalf("got %+v, %v", got, err)
	}
}

func TestNewClientRejectsJunk(t *testing.T) {
	for _, u := range []string{"unix://", "ftp://x", "just-a-path"} {
		if _, err := NewClient(u); err == nil {
			t.Errorf("NewClient(%q) should fail", u)
		}
	}
}
