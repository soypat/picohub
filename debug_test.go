package main

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/soypat/picohub/flash"
)

// stubOpenOCD puts a fake openocd on PATH that behaves the way the real one
// does where this package cares: it announces the gdb port it was told to use
// and then stays up until it is signalled. That is enough to exercise the whole
// session lifecycle without a probe or a real OpenOCD install.
func stubOpenOCD(t *testing.T) { stubOpenOCDLifetime(t, "300") }

// stubOpenOCDLifetime is stubOpenOCD with a stub that exits by itself after
// lifetime seconds with a non-zero status, standing in for an OpenOCD that
// dies mid-session.
func stubOpenOCDLifetime(t *testing.T, lifetime string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("stub needs a POSIX shell")
	}
	dir := t.TempDir()
	script := `#!/bin/sh
if [ "$1" = "--version" ]; then echo "Open On-Chip Debugger 0.12.0-stub"; exit 0; fi
port=3333
for a in "$@"; do
  case "$a" in "gdb_port "*) port="${a##* }";; esac
done
echo "Info : Listening on port $port for gdb connections" >&2
sleep ` + lifetime + `
echo "Error: adapter went away" >&2
exit 3
`
	if err := os.WriteFile(filepath.Join(dir, "openocd"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// newDebugManager returns a manager holding one board that discovery reports as
// present, ignored, and known to be an RP2350.
func newDebugManager(t *testing.T) (*Manager, *managed) {
	t.Helper()
	m := NewManager(newTestStore(t), NewHub(), time.Hour, 1024, DebugOptions{BindTo: "127.0.0.1", PortBase: 34000})
	md := m.get("probe-1", true)
	md.observe(flash.Descriptor{ID: "probe-1", Target: flash.TargetPico2, Port: "/dev/ttyACM0", Present: true}, true, true)
	return m, md
}

func TestDebugSessionLifecycle(t *testing.T) {
	stubOpenOCD(t)
	m, md := newDebugManager(t)

	if err := m.StartDebug(context.Background(), "probe-1"); err != nil {
		t.Fatalf("StartDebug: %v", err)
	}
	v := md.view()
	if v.Busy != busyDebug {
		t.Errorf("busy = %q want %q", v.Busy, busyDebug)
	}
	if v.DebugAddr != "127.0.0.1:34000" {
		t.Errorf("DebugAddr = %q want 127.0.0.1:34000", v.DebugAddr)
	}
	if !strings.Contains(v.DebugLog, "Listening on port 34000") {
		t.Errorf("OpenOCD output not captured: %q", v.DebugLog)
	}

	// A second session must not be able to take a board that already has one.
	if err := m.StartDebug(context.Background(), "probe-1"); err == nil {
		t.Error("starting a second session should be refused")
	}

	if err := m.StopDebug("probe-1"); err != nil {
		t.Fatalf("StopDebug: %v", err)
	}
	// The watcher releases the claim, so give it a moment to run.
	waitFor(t, func() bool { return md.view().Busy == "" }, "claim released after stop")
	if srv := md.debugServer(); srv != nil {
		t.Error("session still registered after stop")
	}
	if v := md.view(); v.DebugAddr != "" {
		t.Errorf("DebugAddr = %q after stop, want empty", v.DebugAddr)
	}
	if err := md.view().LastErr; err != "" {
		t.Errorf("a requested stop should not be reported as an error: %q", err)
	}

	// The board must be usable again.
	if err := m.StopDebug("probe-1"); err == nil {
		t.Error("stopping a board with no session should be refused")
	}
	if err := m.StartDebug(context.Background(), "probe-1"); err != nil {
		t.Fatalf("board not reusable after a session: %v", err)
	}
	if err := m.StopDebug("probe-1"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return md.view().Busy == "" }, "claim released after second stop")
}

// An OpenOCD that dies on its own must free the board exactly as a clicked stop
// does, and must say why.
func TestDebugSessionCrashReleasesBoard(t *testing.T) {
	stubOpenOCDLifetime(t, "0.2")
	m, md := newDebugManager(t)

	if err := m.StartDebug(context.Background(), "probe-1"); err != nil {
		t.Fatalf("StartDebug: %v", err)
	}
	waitFor(t, func() bool { return md.view().Busy == "" }, "claim released after crash")
	if md.debugServer() != nil {
		t.Error("session still registered after crash")
	}
	if md.view().LastErr == "" {
		t.Error("a session that died on its own should report why")
	}
}

// Shutting picohub down must not leave an OpenOCD holding a probe.
func TestShutdownStopsDebugSession(t *testing.T) {
	stubOpenOCD(t)
	m, md := newDebugManager(t)

	if err := m.StartDebug(context.Background(), "probe-1"); err != nil {
		t.Fatalf("StartDebug: %v", err)
	}
	srv := md.debugServer()
	m.shutdown()
	select {
	case <-srv.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("shutdown left OpenOCD running")
	}
}

// A board that is not on the bus has no probe to serve.
func TestStartDebugRequiresPresence(t *testing.T) {
	stubOpenOCD(t)
	m := NewManager(newTestStore(t), NewHub(), time.Hour, 1024, DebugOptions{})
	m.get("probe-1", true) // known, never seen on the bus
	if err := m.StartDebug(context.Background(), "probe-1"); err == nil {
		t.Error("starting a session on an absent board should be refused")
	}
	if err := m.StartDebug(context.Background(), "nope"); err == nil {
		t.Error("starting a session on an unknown board should be refused")
	}
}

// A chip we cannot name must fail with a message rather than a guess, and the
// failure must not leave the board claimed.
func TestStartDebugWithoutTargetReleasesBoard(t *testing.T) {
	stubOpenOCD(t)
	m := NewManager(newTestStore(t), NewHub(), time.Hour, 1024, DebugOptions{})
	md := m.get("probe-1", true)
	md.observe(flash.Descriptor{ID: "probe-1", Target: flash.TargetUnknown, Present: true}, true, false)

	err := m.StartDebug(context.Background(), "probe-1")
	if err == nil {
		t.Fatal("a board with no derivable target should not start a session")
	}
	if !strings.Contains(err.Error(), "target") {
		t.Errorf("error should name the missing target, got %q", err)
	}
	if busy := md.view().Busy; busy != "" {
		t.Errorf("failed start left the board claimed as %q", busy)
	}
}

func waitFor(t *testing.T, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// The debug page has to render both states correctly: the gdb command must
// carry the host the browser used and the port OpenOCD actually bound, and the
// exposure of the port must be stated plainly before a session is started.
func TestDebugPageHTTP(t *testing.T) {
	stubOpenOCD(t)
	store := newTestStore(t)
	hub := NewHub()
	m := NewManager(store, hub, time.Hour, 1024, DebugOptions{BindTo: "0.0.0.0", PortBase: 34100})
	md := m.get("probe-1", true)
	md.observe(flash.Descriptor{ID: "probe-1", Target: flash.TargetPico2, Port: "/dev/ttyACM0", Present: true}, true, true)

	ts := httptest.NewServer(NewServer(store, m, hub).Handler())
	defer ts.Close()
	t.Cleanup(func() { _ = m.StopDebug("probe-1") })

	body := httpGet(t, ts.URL+"/devices/probe-1/debug")
	for _, want := range []string{
		"unauthenticated",     // the exposure is stated before you can start
		"-ocd-bind 127.0.0.1", // and how to avoid it
		`value="cmsis-dap"`,   // config prefilled from the discovered chip
		`value="rp2350"`,
		"debug/start",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("idle page missing %q", want)
		}
	}
	if strings.Contains(body, "extended-remote") {
		t.Error("idle page should not offer a gdb command")
	}

	if code := httpPost(t, ts.URL+"/devices/probe-1/debug/start"); code != http.StatusNoContent {
		t.Fatalf("start returned %d", code)
	}

	// The page is fetched through a host:port, which is what the rendered gdb
	// command must point at — not OpenOCD's bind address.
	body = httpGet(t, ts.URL+"/devices/probe-1/debug")
	host, _, _ := net.SplitHostPort(strings.TrimPrefix(ts.URL, "http://"))
	wantCmd := "target extended-remote " + net.JoinHostPort(host, "34100")
	if !strings.Contains(body, wantCmd) {
		t.Errorf("running page missing %q", wantCmd)
	}
	for _, want := range []string{"Listening on port 34100", "debug/stop", "0.0.0.0:34100"} {
		if !strings.Contains(body, want) {
			t.Errorf("running page missing %q", want)
		}
	}
	if strings.Contains(body, "debug/start") {
		t.Error("running page should not offer to start a second session")
	}

	if code := httpPost(t, ts.URL+"/devices/probe-1/debug/stop"); code != http.StatusNoContent {
		t.Fatalf("stop returned %d", code)
	}
	waitFor(t, func() bool { return md.view().Busy == "" }, "claim released after stop")
}

func httpGet(t *testing.T, url string) string {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: %s", url, resp.Status)
	}
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func httpPost(t *testing.T, url string) int {
	t.Helper()
	resp, err := http.Post(url, "application/x-www-form-urlencoded", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		t.Logf("POST %s: %s: %s", url, resp.Status, strings.TrimSpace(string(b)))
	}
	return resp.StatusCode
}

// An RP2350-based Debug Probe enumerates as 2e8a:000c exactly like the
// RP2040-based one, so discovery classifies both as a plain pico. Pinning the
// family must stick, survive a reconcile pass, and change what OpenOCD is
// configured with.
func TestTargetOverrideForProbe(t *testing.T) {
	store := newTestStore(t)
	hub := NewHub()
	m := NewManager(store, hub, time.Hour, 1024, DebugOptions{})
	const id = "D4A682F186A20C21"
	probe := flash.Descriptor{ID: id, VID: 0x2e8a, PID: 0x000c, Target: flash.TargetPico, Present: true}
	md := m.get(id, true)
	md.observe(probe, true, true)

	if got := md.view().Target; got != flash.TargetPico {
		t.Fatalf("discovery should classify the probe as pico, got %v", got)
	}
	if cfg, _ := m.DebugConfigFor(id); cfg.Target != "rp2040" {
		t.Errorf("default OpenOCD target = %q want rp2040", cfg.Target)
	}

	if err := m.SetTargetOverride(id, flash.TargetPico2); err != nil {
		t.Fatal(err)
	}
	if got := md.view().Target; got != flash.TargetPico2 {
		t.Errorf("pin not visible immediately: %v", got)
	}
	// A reconcile pass re-derives the descriptor from discovery plus the record;
	// the pin has to survive that, which is where it was being lost before.
	rec, _, _ := store.Device(id)
	if got := applyRecord(probe, rec).Target; got != flash.TargetPico2 {
		t.Errorf("pin lost on reconcile: %v", got)
	}
	if cfg, _ := m.DebugConfigFor(id); cfg.Target != "rp2350" {
		t.Errorf("pinned family did not change the OpenOCD target: %q", cfg.Target)
	}

	// Clearing goes back to what discovery says.
	if err := m.SetTargetOverride(id, flash.TargetUnknown); err != nil {
		t.Fatal(err)
	}
	rec, _, _ = store.Device(id)
	if got := applyRecord(probe, rec).Target; got != flash.TargetPico {
		t.Errorf("clearing the pin should restore the discovered family, got %v", got)
	}
}

// The family selector is on the debug page and must round-trip through HTTP.
func TestTargetOverrideHTTP(t *testing.T) {
	store := newTestStore(t)
	hub := NewHub()
	m := NewManager(store, hub, time.Hour, 1024, DebugOptions{})
	const id = "D4A682F186A20C21"
	md := m.get(id, true)
	md.observe(flash.Descriptor{ID: id, VID: 0x2e8a, PID: 0x000c, Target: flash.TargetPico, Present: true}, true, true)

	ts := httptest.NewServer(NewServer(store, m, hub).Handler())
	defer ts.Close()

	if code := httpPostForm(t, ts.URL+"/devices/"+id+"/target", url.Values{"target": {"pico2"}}); code != http.StatusNoContent {
		t.Fatalf("setting the family returned %d", code)
	}
	body := httpGet(t, ts.URL+"/devices/"+id+"/debug")
	if !strings.Contains(body, `value="pico2" selected`) {
		t.Error("debug page does not show the pinned family as selected")
	}
	if !strings.Contains(body, `value="rp2350"`) {
		t.Error("pinned family did not flow into the OpenOCD target field")
	}

	if code := httpPostForm(t, ts.URL+"/devices/"+id+"/target", url.Values{"target": {"not-a-chip"}}); code != http.StatusBadRequest {
		t.Errorf("an unknown family should be rejected, got %d", code)
	}
}

func httpPostForm(t *testing.T, url string, form url.Values) int {
	t.Helper()
	resp, err := http.PostForm(url, form)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		t.Logf("POST %s: %s: %s", url, resp.Status, strings.TrimSpace(string(b)))
	}
	return resp.StatusCode
}
