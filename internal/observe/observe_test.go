package observe

import (
	"context"
	"io"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

func listen(addr string) (net.Listener, error) { return net.Listen("tcp", addr) }

func readCounter(t *testing.T) float64 {
	t.Helper()
	return testutil.ToFloat64(degradedCycles)
}

func TestHealthLiveness(t *testing.T) {
	h := NewHealth(50 * time.Millisecond)
	if !h.live() {
		t.Fatal("fresh Health should be live")
	}
	time.Sleep(80 * time.Millisecond)
	if h.live() {
		t.Fatal("Health should be stale after maxStale with no Beat")
	}
	h.Beat()
	if !h.live() {
		t.Fatal("Beat should restore liveness")
	}
}

func TestHealthReadiness(t *testing.T) {
	h := NewHealth(time.Minute)
	if h.ready.Load() {
		t.Fatal("not ready until Ready() called")
	}
	h.Ready()
	if !h.ready.Load() {
		t.Fatal("Ready() should flip readiness")
	}
}

func TestServeEndpoints(t *testing.T) {
	h := NewHealth(time.Minute)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Ephemeral port via a fixed high addr; retry-free since the test owns it.
	addr := "127.0.0.1:0"
	// Bind explicitly so we know the port.
	ln, err := listen(addr)
	if err != nil {
		t.Fatal(err)
	}
	go h.serveOn(ctx, ln)
	base := "http://" + ln.Addr().String()

	// readyz is 503 before Ready(), 200 after.
	if code := get(t, base+"/readyz"); code != http.StatusServiceUnavailable {
		t.Errorf("readyz before Ready = %d, want 503", code)
	}
	h.Ready()
	if code := get(t, base+"/readyz"); code != http.StatusOK {
		t.Errorf("readyz after Ready = %d, want 200", code)
	}
	// metrics endpoint serves.
	if code := get(t, base+"/metrics"); code != http.StatusOK {
		t.Errorf("metrics = %d, want 200", code)
	}
	// healthz reflects the heartbeat.
	if code := get(t, base+"/healthz"); code != http.StatusOK {
		t.Errorf("healthz fresh = %d, want 200", code)
	}
}

func TestSetCapabilitySourceCountsDegraded(t *testing.T) {
	// Preferred source: no degraded increment; fallback: one each.
	before := readCounter(t)
	SetCapabilitySource("api")
	if readCounter(t) != before {
		t.Error("api source must not count as degraded")
	}
	SetCapabilitySource("index")
	if readCounter(t) != before+1 {
		t.Error("index source must count one degraded cycle")
	}
}

func get(t *testing.T, url string) int {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return resp.StatusCode
}
