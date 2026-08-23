package main

import (
	"path/filepath"
	"testing"
	"time"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	st, err := OpenStore(filepath.Join(dir, "test.db"), filepath.Join(dir, "logs"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func TestStoreDeviceRoundTrip(t *testing.T) {
	st := newTestStore(t)
	now := time.Now()

	rec, err := st.DeviceSeen("dev-1", now)
	if err != nil {
		t.Fatal(err)
	}
	if !rec.FirstSeen.Equal(now) || !rec.LastSeen.Equal(now) {
		t.Errorf("timestamps not set: %+v", rec)
	}

	later := now.Add(time.Minute)
	rec2, _ := st.DeviceSeen("dev-1", later)
	if !rec2.FirstSeen.Equal(now) {
		t.Errorf("FirstSeen should be preserved, got %v", rec2.FirstSeen)
	}
	if !rec2.LastSeen.Equal(later) {
		t.Errorf("LastSeen should update, got %v", rec2.LastSeen)
	}

	if err := st.SetDeviceName("dev-1", "my pico"); err != nil {
		t.Fatal(err)
	}
	got, found, _ := st.Device("dev-1")
	if !found || got.Name != "my pico" {
		t.Errorf("name not persisted: %+v found=%v", got, found)
	}
	if !got.FirstSeen.Equal(now) {
		t.Errorf("rename clobbered FirstSeen: %v", got.FirstSeen)
	}
}

func TestStoreDeviceIgnored(t *testing.T) {
	st := newTestStore(t)
	now := time.Now()

	if _, err := st.DeviceSeen("dev-1", now); err != nil {
		t.Fatal(err)
	}
	if err := st.SetDeviceIgnored("dev-1", true); err != nil {
		t.Fatal(err)
	}
	// Being seen again must not clear the flag: discovery reports presence, it
	// does not get a say in policy.
	rec, _ := st.DeviceSeen("dev-1", now.Add(time.Minute))
	if !rec.Ignored {
		t.Error("DeviceSeen cleared the ignore flag")
	}

	// A device may be marked ignored before discovery has ever reported it.
	if err := st.SetDeviceIgnored("dev-2", true); err != nil {
		t.Fatal(err)
	}
	got, found, _ := st.Device("dev-2")
	if !found || !got.Ignored {
		t.Errorf("ignore flag not persisted for an unseen device: %+v found=%v", got, found)
	}
}

func TestStoreSessions(t *testing.T) {
	st := newTestStore(t)

	s1, err := st.SessionStart("dev-1", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if s1.Seq != 1 {
		t.Errorf("first session seq=%d want 1", s1.Seq)
	}
	if !s1.Active() {
		t.Error("new session should be active")
	}

	s2, _ := st.SessionStart("dev-1", time.Now())
	if s2.Seq != 2 {
		t.Errorf("second session seq=%d want 2", s2.Seq)
	}

	if err := st.SessionUpdate(s1.ID, func(s *Session) {
		s.EndedAt = time.Now()
		s.ByteLen = 4096
	}); err != nil {
		t.Fatal(err)
	}
	got, found, _ := st.Session(s1.ID)
	if !found || got.ByteLen != 4096 || got.Active() {
		t.Errorf("session update not persisted: %+v", got)
	}

	sessions, _ := st.Sessions("dev-1")
	if len(sessions) != 2 {
		t.Fatalf("want 2 sessions, got %d", len(sessions))
	}
	if sessions[0].Seq != 2 || sessions[1].Seq != 1 {
		t.Errorf("sessions should be newest-first: %d,%d", sessions[0].Seq, sessions[1].Seq)
	}
}

func TestStoreFlashes(t *testing.T) {
	st := newTestStore(t)
	t0 := time.Now()
	if err := st.FlashRecord(FlashRecord{DeviceID: "dev-1", At: t0, Firmware: "a.uf2", OK: true}); err != nil {
		t.Fatal(err)
	}
	if err := st.FlashRecord(FlashRecord{DeviceID: "dev-1", At: t0.Add(time.Second), Firmware: "b.uf2", OK: false, Err: "boom"}); err != nil {
		t.Fatal(err)
	}
	if err := st.FlashRecord(FlashRecord{DeviceID: "dev-2", At: t0, Firmware: "c.uf2"}); err != nil {
		t.Fatal(err)
	}

	flashes, _ := st.Flashes("dev-1")
	if len(flashes) != 2 {
		t.Fatalf("want 2 flashes for dev-1, got %d", len(flashes))
	}
	if flashes[0].Firmware != "b.uf2" {
		t.Errorf("flashes should be newest-first, got %q first", flashes[0].Firmware)
	}
}
