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
	"strings"
	"time"

	"golang.org/x/sys/unix"
	"k8s.io/klog/v2"
)

// subsystems we re-probe for. Everything else (usb, block, …) is noise.
var subsystems = map[string]bool{
	"drm":   true,
	"accel": true,
	"pci":   true, // bind/unbind arrives on the pci subsystem
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

	events := make(chan struct{}, 1)
	go func() {
		defer unix.Close(fd)
		go func() { // unblock the read loop on shutdown
			<-ctx.Done()
			unix.Close(fd)
		}()
		buf := make([]byte, 4096)
		var last time.Time
		for {
			n, err := unix.Read(fd, buf)
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				klog.ErrorS(err, "uevent read failed; hotplug listener stopping")
				return
			}
			ev, ok := parseUevent(buf[:n])
			if !ok || !relevant(ev) {
				continue
			}
			// Coalesce bursts (one hotplug = many uevents).
			if time.Since(last) < debounce {
				continue
			}
			last = time.Now()
			klog.V(2).InfoS("accelerator uevent", "action", ev.action, "subsystem", ev.subsystem)
			select {
			case events <- struct{}{}:
			default: // a re-probe is already pending
			}
		}
	}()
	return events, nil
}
