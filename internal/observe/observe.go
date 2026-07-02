// Package observe is the driver's operator-facing surface: Prometheus
// metrics plus liveness/readiness endpoints on one HTTP server. The driver
// is built to degrade quietly (last-known-good capability cache, index
// fallback, vendorManaged demotion); these metrics make those transitions
// visible at fleet scale, and the health endpoints turn a hung reconcile
// loop from a silent 1/1-Running outage into a failing probe.
package observe

import (
	"context"
	"net"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"k8s.io/klog/v2"
)

const subsystem = "llmfit_dra"

var (
	// capabilitySource: 1 on the active transport, 0 on the others — so a
	// fleet-wide "N nodes fell back to the index" is a single query.
	capabilitySource = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Subsystem: subsystem, Name: "capability_source",
		Help: "Active capability transport (1=active): api|exec|cache|index|none.",
	}, []string{"source"})

	degradedCycles = promauto.NewCounter(prometheus.CounterOpts{
		Subsystem: subsystem, Name: "degraded_cycles_total",
		Help: "Probe cycles served from a non-preferred capability source (exec/cache/index).",
	})

	probeDuration = promauto.NewHistogram(prometheus.HistogramOpts{
		Subsystem: subsystem, Name: "probe_duration_seconds",
		Help:    "Wall time of one probe→publish cycle.",
		Buckets: prometheus.DefBuckets,
	})

	sliceUpdates = promauto.NewCounter(prometheus.CounterOpts{
		Subsystem: subsystem, Name: "slice_updates_total",
		Help: "Cycles where published inventory changed (an actual API write).",
	})

	prepareTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Subsystem: subsystem, Name: "prepare_total",
		Help: "NodePrepareResources results by outcome.",
	}, []string{"result"})

	unprepareTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Subsystem: subsystem, Name: "unprepare_total",
		Help: "NodeUnprepareResources results by outcome.",
	}, []string{"result"})
)

// knownSources seeds the gauge so every series exists at 0 before the first
// probe — dashboards and alerts don't have to special-case a missing label.
var knownSources = []string{"api", "exec", "cache", "index", "none"}

// SetCapabilitySource marks src active (1) and all others idle (0), and
// counts a degraded cycle when the source is not the preferred "api".
func SetCapabilitySource(src string) {
	for _, s := range knownSources {
		v := 0.0
		if s == src {
			v = 1
		}
		capabilitySource.WithLabelValues(s).Set(v)
	}
	switch src {
	case "exec", "cache", "index":
		degradedCycles.Inc()
	}
}

// ObserveProbe records one cycle's duration and whether it wrote.
func ObserveProbe(d time.Duration, changed bool) {
	probeDuration.Observe(d.Seconds())
	if changed {
		sliceUpdates.Inc()
	}
}

// Prepare/Unprepare record plugin outcomes ("ok" or "error").
func Prepare(result string)   { prepareTotal.WithLabelValues(result).Inc() }
func Unprepare(result string) { unprepareTotal.WithLabelValues(result).Inc() }

// Health tracks liveness (reconcile-loop heartbeat) and readiness (startup
// complete). It is shared with main's loop and the HTTP handlers.
type Health struct {
	lastBeat atomic.Int64 // unix nanos of the last successful cycle
	ready    atomic.Bool
	maxStale time.Duration // liveness fails if no beat within this window
}

func NewHealth(maxStale time.Duration) *Health {
	h := &Health{maxStale: maxStale}
	h.lastBeat.Store(time.Now().UnixNano())
	return h
}

// Beat marks a successful reconcile cycle (liveness).
func (h *Health) Beat() { h.lastBeat.Store(time.Now().UnixNano()) }

// Ready flips readiness true once (after publisher + plugin start).
func (h *Health) Ready() { h.ready.Store(true) }

func (h *Health) live() bool {
	return time.Since(time.Unix(0, h.lastBeat.Load())) <= h.maxStale
}

// Serve starts the metrics+health HTTP server and stops it on ctx cancel.
// A failure to listen is returned so the driver can decide whether to fail
// hard (it does — observability is not optional in this deployment).
func (h *Health) Serve(ctx context.Context, addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	return h.serveOn(ctx, ln)
}

func (h *Health) serveOn(ctx context.Context, ln net.Listener) error {
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.Handler())
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		if h.live() {
			w.WriteHeader(http.StatusOK)
			return
		}
		http.Error(w, "reconcile loop stalled", http.StatusServiceUnavailable)
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		if h.ready.Load() {
			w.WriteHeader(http.StatusOK)
			return
		}
		http.Error(w, "not ready", http.StatusServiceUnavailable)
	})
	srv := &http.Server{Handler: mux}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()
	klog.InfoS("serving metrics and health", "addr", ln.Addr().String())
	if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}
