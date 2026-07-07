// Package hotplug listens for kernel uevents (NETLINK_KOBJECT_UEVENT) and
// signals when an accelerator-relevant device event occurs, so the probe
// loop can re-walk immediately instead of waiting out its ticker. This is
// both the hot-attach path (card appears/disappears) and the event-driven
// health path (bind/unbind, error events re-read driver binding and RAS
// counters within seconds).
//
// The uevent netlink socket is network-namespace scoped: the DaemonSet
// must run with hostNetwork to see host device events.
package hotplug

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"
	"k8s.io/klog/v2"
)

// subsystems we re-probe for. Everything else is noise — including "pci",
// which would match every NIC/NVMe flap on the node: GPU/NPU appearance,
// removal, and driver bind/unbind all surface as drm/accel class events.
var subsystems = map[string]bool{
	"drm":   true,
	"accel": true,
}

// uevent is one parsed kernel message.
type uevent struct {
	action    string // add | remove | change | bind | unbind | …
	subsystem string
}

// parseUevent decodes the kernel's "action@devpath\0KEY=VAL\0…" format.
// Returns ok=false for messages without the header (e.g. udevd's own
// libudev-prefixed rebroadcasts, which duplicate the kernel ones).
func parseUevent(buf []byte) (uevent, bool) {
	fields := strings.Split(string(buf), "\x00")
	if len(fields) == 0 || !strings.Contains(fields[0], "@") {
		return uevent{}, false
	}
	ev := uevent{action: strings.SplitN(fields[0], "@", 2)[0]}
	for _, f := range fields[1:] {
		if v, ok := strings.CutPrefix(f, "SUBSYSTEM="); ok {
			ev.subsystem = v
		}
	}
	return ev, true
}

// relevant reports whether the event should trigger a re-probe.
func relevant(ev uevent) bool {
	return subsystems[ev.subsystem]
}

// readAction classifies a netlink read error into how the loop should react.
type readAction int

const (
	actRetry   readAction = iota // interrupted / would-block: read again
	actOverrun                   // kernel dropped messages: force a re-probe, read again
	actFatal                     // unrecoverable: stop the listener (ticker still reconciles)
)

func classifyReadErr(err error) readAction {
	switch {
	case errors.Is(err, unix.EINTR), errors.Is(err, unix.EAGAIN):
		return actRetry
	case errors.Is(err, unix.ENOBUFS):
		return actOverrun
	default:
		return actFatal
	}
}

// Listen opens the uevent socket and sends one (coalesced) signal per burst
// of relevant events. The returned channel has capacity 1 and never blocks
// the reader: signals arriving while a re-probe is pending are folded into
// it. Returns an error only if the socket can't be opened (no privileges,
// no hostNetwork) — callers should fall back to ticker-only probing.
func Listen(ctx context.Context, debounce time.Duration) (<-chan struct{}, error) {
	fd, err := unix.Socket(unix.AF_NETLINK, unix.SOCK_RAW|unix.SOCK_CLOEXEC, unix.NETLINK_KOBJECT_UEVENT)
	if err != nil {
		return nil, err
	}
	// Group 1 = kernel uevents (group 2 is udevd's rebroadcast).
	if err := unix.Bind(fd, &unix.SockaddrNetlink{Family: unix.AF_NETLINK, Groups: 1}); err != nil {
		unix.Close(fd)
		return nil, err
	}

	// A bigger receive buffer survives event storms longer before ENOBUFS.
	_ = unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_RCVBUFFORCE, 1<<20)

	// One close path via Once: both the shutdown watcher (which closes the
	// fd to unblock a pending Read) and the reader's own exit funnel through
	// it, so the fd is never double-closed and the watcher never leaks.
	var closeOnce sync.Once
	closeFd := func() { closeOnce.Do(func() { unix.Close(fd) }) }
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
		case <-done: // reader exited on its own (fatal read error)
		}
		closeFd()
	}()

	raw := make(chan uevent, 16)
	events := make(chan struct{}, 1)
	go func() {
		defer close(raw)  // terminates the debounce goroutine
		defer close(done) // stops the watcher
		defer closeFd()
		buf := make([]byte, 4096)
		for {
			n, err := unix.Read(fd, buf)
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				switch classifyReadErr(err) {
				case actRetry:
					// EINTR (Go's async preemption raises it routinely on a
					// blocking raw syscall) / EAGAIN — not failures, read again.
					continue
				case actOverrun:
					// The kernel dropped messages during a storm — exactly when
					// we must keep listening. Force a re-probe (a relevant event
					// may have been among the dropped) and read on.
					klog.V(2).InfoS("uevent overrun; forcing re-probe and continuing")
					select {
					case raw <- uevent{action: "overrun", subsystem: "drm"}:
					default:
					}
					continue
				default:
					klog.ErrorS(err, "uevent read failed; hotplug listener stopping (ticker still reconciles)")
					return
				}
			}
			ev, ok := parseUevent(buf[:n])
			if !ok || !relevant(ev) {
				continue
			}
			select {
			case raw <- ev:
			default: // coalescer is behind; the pending signal covers it
			}
		}
	}()
	// Trailing-edge debounce: a burst (one hotplug = many uevents) produces
	// ONE signal after the burst settles, so the re-probe sees the final
	// state instead of a half-initialized device. Closing events on exit is
	// the caller's only signal that hotplug died mid-run (fatal read error)
	// and probing degraded to ticker-only.
	go func() {
		defer close(events)
		for ev := range raw {
			klog.V(2).InfoS("accelerator uevent", "action", ev.action, "subsystem", ev.subsystem)
			timer := time.NewTimer(debounce)
		drain:
			for {
				select {
				case _, ok := <-raw:
					if !ok {
						break drain
					}
					timer.Reset(debounce)
				case <-timer.C:
					break drain
				}
			}
			timer.Stop()
			select {
			case events <- struct{}{}:
			default: // a re-probe is already pending
			}
		}
	}()
	return events, nil
}
