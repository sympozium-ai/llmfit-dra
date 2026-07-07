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
	"strconv"
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
		Help: "NodePrepareResources results by outcome and reason (none|no_allocation|device_missing|foreign_only|cdi_write).",
	}, []string{"result", "reason"})

	unprepareTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Subsystem: subsystem, Name: "unprepare_total",
		Help: "NodeUnprepareResources results by outcome.",
	}, []string{"result"})

	prepareDuration = promauto.NewHistogram(prometheus.HistogramOpts{
		Subsystem: subsystem, Name: "prepare_duration_seconds",
		Help:    "Wall time of one claim's prepare (CDI spec write included).",
		Buckets: prometheus.DefBuckets,
	})

	unprepareDuration = promauto.NewHistogram(prometheus.HistogramOpts{
		Subsystem: subsystem, Name: "unprepare_duration_seconds",
		Help:    "Wall time of one claim's unprepare.",
		Buckets: prometheus.DefBuckets,
	})

	// devices is the published inventory itself — the thing that differs
	// across a heterogeneous fleet. Reset+set each probe cycle, so a device
	// disappearing is visible as the series going to 0, and
	// sum(llmfit_dra_devices{vendor="amd",healthy="true"}) is a fleet fact.
	devices = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Subsystem: subsystem, Name: "devices",
		Help: "Devices in the published inventory by kind/vendor/driver/health.",
	}, []string{"kind", "vendor", "driver", "healthy"})

	devicesVendorManaged = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Subsystem: subsystem, Name: "devices_vendor_managed",
		Help: "Published devices demoted to fitness-only (vendor DRA driver owns allocation, or the kernel driver is unpreparable).",
	}, []string{"kind", "driver"})

	probeErrors = promauto.NewCounterVec(prometheus.CounterOpts{
		Subsystem: subsystem, Name: "probe_errors_total",
		Help: "Probe/publish cycle failures by stage (walk|meminfo|detect|coexist).",
	}, []string{"stage"})

	slicePublishErrors = promauto.NewCounterVec(prometheus.CounterOpts{
		Subsystem: subsystem, Name: "slice_publish_errors_total",
		Help: "ResourceSlice write failures reported by the helper controller (api|dropped_fields).",
	}, []string{"kind"})

	hotplugListenerUp = promauto.NewGauge(prometheus.GaugeOpts{
		Subsystem: subsystem, Name: "hotplug_listener_up",
		Help: "1 while the uevent listener runs; 0 when probing is ticker-only.",
	})

	hotplugWakeups = promauto.NewCounter(prometheus.CounterOpts{
		Subsystem: subsystem, Name: "hotplug_wakeups_total",
		Help: "Re-probe cycles triggered by kernel uevents (vs the ticker).",
	})

	cdiOrphansRemoved = promauto.NewCounter(prometheus.CounterOpts{
		Subsystem: subsystem, Name: "cdi_orphans_removed_total",
		Help: "Orphaned CDI spec files removed by startup GC.",
	})

	// Controller-mode metrics (the ModelClaim reconciler).
	resolveDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Subsystem: subsystem, Name: "resolve_duration_seconds",
		Help:    "llmfit claim resolve wall time by result (ok|error|cached).",
		Buckets: prometheus.DefBuckets,
	}, []string{"result"})

	modelClaimReconciles = promauto.NewCounterVec(prometheus.CounterOpts{
		Subsystem: subsystem, Name: "modelclaim_reconciles_total",
		Help: "ModelClaim reconcile outcomes.",
	}, []string{"result"})
)

// ObserveResolve records one resolver call ("ok"|"error"|"cached").
func ObserveResolve(d time.Duration, result string) {
	resolveDuration.WithLabelValues(result).Observe(d.Seconds())
}

// ModelClaimReconcile counts one reconcile outcome ("ok"|"error").
func ModelClaimReconcile(result string) {
	modelClaimReconciles.WithLabelValues(result).Inc()
}

// DeviceInfo is the label set SetInventory aggregates. Values must stay
// bounded: kind and driver come from a small kernel-driver set, vendor from
// vendorName's enumeration — never device names or PCI addresses.
type DeviceInfo struct {
	Kind, Vendor, Driver   string
	Healthy, VendorManaged bool
}

// SetInventory replaces the published-inventory gauges with this cycle's
// devices. Reset first: a stale series for removed hardware would report a
// device that no longer exists.
func SetInventory(devs []DeviceInfo) {
	devices.Reset()
	devicesVendorManaged.Reset()
	counts := map[DeviceInfo]int{}
	vm := map[[2]string]int{}
	for _, d := range devs {
		key := DeviceInfo{Kind: d.Kind, Vendor: d.Vendor, Driver: d.Driver, Healthy: d.Healthy}
		counts[key]++
		if d.VendorManaged {
			vm[[2]string{d.Kind, d.Driver}]++
		}
	}
	for k, n := range counts {
		devices.WithLabelValues(k.Kind, k.Vendor, k.Driver, strconv.FormatBool(k.Healthy)).Set(float64(n))
	}
	for k, n := range vm {
		devicesVendorManaged.WithLabelValues(k[0], k[1]).Set(float64(n))
	}
}

// ProbeError counts a failed stage of the probe/publish cycle.
func ProbeError(stage string) { probeErrors.WithLabelValues(stage).Inc() }

// SlicePublishError counts a ResourceSlice write failure surfaced by the
// upstream helper's ErrorHandler.
func SlicePublishError(kind string) { slicePublishErrors.WithLabelValues(kind).Inc() }

// HotplugListener records whether uevent-driven probing is active.
func HotplugListener(up bool) {
	if up {
		hotplugListenerUp.Set(1)
	} else {
		hotplugListenerUp.Set(0)
	}
}

// HotplugWakeup counts a uevent-triggered re-probe.
func HotplugWakeup() { hotplugWakeups.Inc() }

// CDIOrphanRemoved counts one orphaned spec removed by startup GC.
func CDIOrphanRemoved() { cdiOrphansRemoved.Inc() }

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

// Prepare records one claim's prepare outcome ("ok"/"error"), the error
// reason ("none" when ok), and its duration.
func Prepare(result, reason string, d time.Duration) {
	prepareTotal.WithLabelValues(result, reason).Inc()
	prepareDuration.Observe(d.Seconds())
}

// Unprepare records one claim's unprepare outcome and duration.
func Unprepare(result string, d time.Duration) {
	unprepareTotal.WithLabelValues(result).Inc()
	unprepareDuration.Observe(d.Seconds())
}

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
