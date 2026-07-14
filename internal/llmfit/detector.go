package llmfit

import (
	"context"
	"fmt"
	"sync"
	"time"

	"k8s.io/klog/v2"
)

// Detector is the capability source the driver actually consumes. Order of
// preference per cycle: the serve API (persistent process, versioned
// contract), the exec fallback (`<bin> --json system`), then the last
// known good result within a staleness bound. The cache is what keeps a
// transient llmfit hiccup from flapping published attributes
// (source llmfit→index→llmfit) fleet-wide — slices only degrade to the
// index after llmfit has been unreachable for longer than maxStale.
type Detector struct {
	api      func(context.Context) (*System, error) // nil when no URL configured
	exec     func(context.Context) (*System, error) // nil when bin is empty
	maxStale time.Duration

	mu        sync.Mutex
	last      *System
	lastAt    time.Time
	transport string // last successful transport, for change logging
}

// NewDetector wires the real transports. url and bin may each be empty;
// with both empty every Detect returns an error (callers fall back to the
// embedded index).
func NewDetector(url, bin string, maxStale time.Duration) (*Detector, error) {
	d := &Detector{maxStale: maxStale}
	if url != "" {
		c, err := NewClient(url)
		if err != nil {
			return nil, err
		}
		d.api = c.Detect
	}
	if bin != "" {
		d.exec = func(ctx context.Context) (*System, error) { return Detect(ctx, bin) }
	}
	return d, nil
}

// Detect returns the freshest capability assessment available.
func (d *Detector) Detect(ctx context.Context) (*System, error) {
	var firstErr error
	for _, t := range []struct {
		name string
		fn   func(context.Context) (*System, error)
	}{{"api", d.api}, {"exec", d.exec}} {
		if t.fn == nil {
			continue
		}
		sys, err := t.fn(ctx)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		d.mu.Lock()
		if d.transport != t.name {
			klog.InfoS("llmfit capability transport", "transport", t.name)
			d.transport = t.name
		}
		backfillBandwidth(sys, d.last)
		d.last, d.lastAt = sys, time.Now()
		d.mu.Unlock()
		return sys, nil
	}

	d.mu.Lock()
	defer d.mu.Unlock()
	if d.last != nil && time.Since(d.lastAt) <= d.maxStale {
		if d.transport != "cache" {
			klog.ErrorS(firstErr, "llmfit unreachable; serving last known good", "age", time.Since(d.lastAt).Round(time.Second))
			d.transport = "cache"
		}
		return d.last, nil
	}
	if firstErr == nil {
		firstErr = fmt.Errorf("no llmfit transport configured")
	}
	return nil, firstErr
}

// backfillBandwidth carries memory_bandwidth_gbps forward from the previous
// reading when a fresh one omits it for the same GPU name. Transports can
// report partial data — llmfit <1.1.3's REST API omitted the field that the
// CLI included (AlexsJones/llmfit#747), so the exec→api handoff erased the
// attribute from published ResourceSlices and flipped bandwidth-bounded
// claims unsatisfiable (issue #38). Bandwidth is a physical constant of the
// GPU model, so a name-keyed carry-forward can never serve wrong data, only
// heal missing data. The merge is per-field on purpose: a source that wins
// the transport preference must not clobber facts it does not know.
func backfillBandwidth(fresh, prev *System) {
	if fresh == nil || prev == nil {
		return
	}
	byName := map[string]*float64{}
	for i := range prev.GPUs {
		if g := &prev.GPUs[i]; g.MemoryBandwidthGBps != nil && *g.MemoryBandwidthGBps > 0 {
			byName[g.Name] = g.MemoryBandwidthGBps
		}
	}
	for i := range fresh.GPUs {
		g := &fresh.GPUs[i]
		if g.MemoryBandwidthGBps == nil || *g.MemoryBandwidthGBps <= 0 {
			if bw, ok := byName[g.Name]; ok {
				g.MemoryBandwidthGBps = bw
			}
		}
	}
}

// Transport reports the transport that served the most recent successful
// Detect ("api" | "exec" | "cache"), for metrics. Empty before the first
// success.
func (d *Detector) Transport() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.transport
}
