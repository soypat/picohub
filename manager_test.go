package main

import (
	"testing"

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
