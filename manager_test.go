package main

import (
	"testing"
	"time"

	"github.com/soypat/picohub/flash"
)

// wantAttached is the whole attach policy, so it is worth pinning down without
// a board on the bus.
func TestWantAttached(t *testing.T) {
	tests := []struct {
		name    string
		desc    flash.Descriptor
		ignored bool
		want    bool
	}{
		{"console board", flash.Descriptor{Present: true}, false, true},
		{"console board, ignored", flash.Descriptor{Present: true}, true, false},
		{"bootsel board", flash.Descriptor{Present: true, BootSel: true}, false, false},
		{"bootsel board, ignored", flash.Descriptor{Present: true, BootSel: true}, true, false},
		{"off the bus", flash.Descriptor{}, false, false},
		{"off the bus, ignored", flash.Descriptor{}, true, false},
	}
	for _, tt := range tests {
		if got := wantAttached(tt.desc, tt.ignored); got != tt.want {
			t.Errorf("%s: wantAttached = %v, want %v", tt.name, got, tt.want)
		}
	}
}

// observe must never let discovery decide policy, and must keep a board's
// identity when it drops off the bus — the UI still lists it.
func TestObserve(t *testing.T) {
	md := &managed{}
	desc := flash.Descriptor{ID: "dev-1", Name: "board", Port: "/dev/ttyACM0", Present: true}

	if !md.observe(desc, true, false) {
		t.Error("first sight should report a presence change")
	}
	if md.presence != OnConsole {
		t.Errorf("presence = %v, want %v", md.presence, OnConsole)
	}
	if md.observe(desc, true, false) {
		t.Error("an unchanged presence should not report a change")
	}

	if !md.observe(flash.Descriptor{}, false, true) {
		t.Error("vanishing should report a presence change")
	}
	if md.presence != Gone {
		t.Errorf("presence = %v, want %v", md.presence, Gone)
	}
	if md.desc.ID != "dev-1" || md.desc.Name != "board" {
		t.Errorf("identity lost when the board vanished: %+v", md.desc)
	}
	if md.desc.Port != "" {
		t.Errorf("stale port %q kept after the board vanished", md.desc.Port)
	}
	if !md.ignored {
		t.Error("ignore flag not mirrored onto the device")
	}
}

// The OpenOCD target must never be guessed for a chip we cannot name: an empty
// target makes StartDebug fail with a message instead of attaching to the
// wrong core.
func TestDefaultDebugConfig(t *testing.T) {
	tests := []struct {
		target     flash.Target
		wantTarget string
	}{
		{flash.TargetPico, "rp2040"},
		{flash.TargetPico2, "rp2350"},
		{flash.TargetESP32C3, ""},
		{flash.TargetUnknown, ""},
	}
	for _, tt := range tests {
		got := defaultDebugConfig(flash.Descriptor{Target: tt.target})
		if got.Target != tt.wantTarget {
			t.Errorf("%v: target = %q want %q", tt.target, got.Target, tt.wantTarget)
		}
		if got.Interface != "cmsis-dap" {
			t.Errorf("%v: interface = %q want cmsis-dap", tt.target, got.Interface)
		}
	}
}

// Stored settings overlay the defaults field by field, so setting only one of
// them keeps the derived value for the others.
func TestEffectiveDebugConfig(t *testing.T) {
	desc := flash.Descriptor{Target: flash.TargetPico2}

	got := effectiveDebugConfig(desc, DeviceRecord{})
	if got.Interface != "cmsis-dap" || got.Target != "rp2350" || got.SpeedKHz != 0 {
		t.Errorf("empty record should give defaults, got %+v", got)
	}

	got = effectiveDebugConfig(desc, DeviceRecord{Debug: DebugConfig{Interface: "picoprobe"}})
	if got.Interface != "picoprobe" || got.Target != "rp2350" {
		t.Errorf("partial override lost the derived target: %+v", got)
	}

	got = effectiveDebugConfig(desc, DeviceRecord{Debug: DebugConfig{Target: "rp2040", SpeedKHz: 5000}})
	if got.Interface != "cmsis-dap" || got.Target != "rp2040" || got.SpeedKHz != 5000 {
		t.Errorf("override not applied: %+v", got)
	}
}

// A fabricated id must never be handed to OpenOCD as an adapter serial.
func TestProbeSerial(t *testing.T) {
	tests := []struct{ id, want string }{
		{"E6614103E76A5D2F", "E6614103E76A5D2F"},
		{"2e8a:000a@1-10", ""}, // stableID's fallback, not a serial
		{"2e8a:0004", ""},
	}
	for _, tt := range tests {
		if got := probeSerial(tt.id); got != tt.want {
			t.Errorf("probeSerial(%q) = %q want %q", tt.id, got, tt.want)
		}
	}
}

// An ignored board is one picohub must not open a console on. A debug session
// claims a different resource, so it must still be able to take the board.
func TestClaimAllowIgnored(t *testing.T) {
	m := NewManager(newTestStore(t), NewHub(), time.Second, 1024, DebugOptions{})
	md := m.get("dev-1", true)
	md.observe(flash.Descriptor{ID: "dev-1", Present: true}, true, true)

	if _, _, err := m.claim("dev-1", busyFlashing, false); err == nil {
		t.Error("flashing an ignored board should be refused")
	}
	md2, _, err := m.claim("dev-1", busyDebug, true)
	if err != nil {
		t.Fatalf("debugging an ignored board should be allowed: %v", err)
	}
	if got := md2.view().Busy; got != busyDebug {
		t.Errorf("busy = %q want %q", got, busyDebug)
	}
	m.release(md2)

	// The claim must actually have been released.
	md3, _, err := m.claim("dev-1", busyDebug, true)
	if err != nil {
		t.Fatalf("claim after release: %v", err)
	}
	m.release(md3)
}
