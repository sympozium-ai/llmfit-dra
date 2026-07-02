package hotplug

import "testing"

func msg(parts ...string) []byte {
	var b []byte
	for _, p := range parts {
		b = append(b, p...)
		b = append(b, 0)
	}
	return b
}

func TestParseUevent(t *testing.T) {
	ev, ok := parseUevent(msg("change@/devices/pci0000:00/0000:01:00.0/drm/card1",
		"ACTION=change", "DEVPATH=/devices/pci0000:00/0000:01:00.0/drm/card1", "SUBSYSTEM=drm"))
	if !ok {
		t.Fatal("expected valid kernel uevent")
	}
	if ev.action != "change" || ev.subsystem != "drm" {
		t.Errorf("parsed %+v", ev)
	}
}

func TestParseUeventRejectsLibudevRebroadcast(t *testing.T) {
	// udevd rebroadcasts start with "libudev" magic, not "action@devpath".
	if _, ok := parseUevent(msg("libudev", "ACTION=change", "SUBSYSTEM=drm")); ok {
		t.Error("libudev-prefixed message must be ignored (would double-fire)")
	}
}

func TestRelevantSubsystems(t *testing.T) {
	// pci is deliberately NOT relevant: it would match every NIC/NVMe flap;
	// accelerator lifecycle surfaces on the drm/accel class events.
	cases := map[string]bool{"drm": true, "accel": true, "pci": false, "usb": false, "block": false, "": false}
	for sub, want := range cases {
		if got := relevant(uevent{action: "add", subsystem: sub}); got != want {
			t.Errorf("relevant(%q) = %v, want %v", sub, got, want)
		}
	}
}
