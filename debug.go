package main

import (
	"context"
	"fmt"
	"html"
	"log/slog"
	"net"
	"strings"

	"github.com/soypat/picohub/flash"
	"github.com/soypat/picohub/ocd"
)

// debugRingSize is how much OpenOCD output is kept for the debug page. Its
// output is a few dozen lines per session, so this is generous.
const debugRingSize = 16 << 10

// defaultDebugConfig derives an OpenOCD configuration from what discovery knows
// about a board. The interface default covers the Raspberry Pi Debug Probe and
// most DAPLink clones; a Pico running picoprobe firmware may need "picoprobe".
// The target is left empty for a board whose chip we cannot name, so a session
// fails with a clear message instead of guessing wrong.
func defaultDebugConfig(desc flash.Descriptor) DebugConfig {
	cfg := DebugConfig{Interface: ocd.DefaultInterface}
	switch desc.Target {
	case flash.TargetPico:
		cfg.Target = "rp2040"
	case flash.TargetPico2:
		cfg.Target = "rp2350"
	}
	return cfg
}

// effectiveDebugConfig overlays a device's stored configuration on the defaults
// derived from the board. Each field is overlaid on its own, so setting only
// the target keeps the default interface.
func effectiveDebugConfig(desc flash.Descriptor, rec DeviceRecord) DebugConfig {
	cfg := defaultDebugConfig(desc)
	if rec.Debug.Interface != "" {
		cfg.Interface = rec.Debug.Interface
	}
	if rec.Debug.Target != "" {
		cfg.Target = rec.Debug.Target
	}
	if rec.Debug.SpeedKHz != 0 {
		cfg.SpeedKHz = rec.Debug.SpeedKHz
	}
	return cfg
}

// probeSerial returns the probe's USB serial number, or "" when the board did
// not report one. stableID falls back to a "vid:pid@usb-path" form for those,
// which is not a serial and must not be passed to OpenOCD as one.
func probeSerial(id string) string {
	if strings.ContainsAny(id, ":@") {
		return ""
	}
	return id
}

// DebugConfigFor returns the OpenOCD configuration a device would be debugged
// with: its stored settings overlaid on the defaults for its target.
func (m *Manager) DebugConfigFor(id string) (DebugConfig, error) {
	md := m.get(id, false)
	if md == nil {
		return DebugConfig{}, fmt.Errorf("unknown device %q", id)
	}
	md.mu.Lock()
	desc := md.desc
	md.mu.Unlock()
	rec, _, err := m.store.Device(id)
	if err != nil {
		return DebugConfig{}, err
	}
	return effectiveDebugConfig(desc, rec), nil
}

// SetDebugConfig persists a device's OpenOCD configuration. It is validated
// here so a bad name is rejected at the form rather than at the next start.
func (m *Manager) SetDebugConfig(id string, cfg DebugConfig) error {
	md := m.get(id, false)
	if md == nil {
		return fmt.Errorf("unknown device %q", id)
	}
	md.mu.Lock()
	desc := md.desc
	md.mu.Unlock()
	rec, _, err := m.store.Device(id)
	if err != nil {
		return err
	}
	rec.Debug = cfg
	if _, err := m.ocdConfig(desc, effectiveDebugConfig(desc, rec), 0); err != nil {
		return err
	}
	if err := m.store.SetDeviceDebug(id, cfg); err != nil {
		return err
	}
	slog.Info("debug config set", "device", id, "interface", cfg.Interface, "target", cfg.Target, "speed_khz", cfg.SpeedKHz)
	m.hub.Publish(sseMessage{Event: eventDevices})
	return nil
}

// ocdConfig builds the OpenOCD configuration for a session and validates it.
// port may be 0 while only validating.
func (m *Manager) ocdConfig(desc flash.Descriptor, cfg DebugConfig, port int) (ocd.Config, error) {
	if cfg.Target == "" {
		return ocd.Config{}, fmt.Errorf("no OpenOCD target set for %q and none could be derived from %s; set one on the debug page", desc.ID, desc.Target)
	}
	c := ocd.Config{
		Binary:     m.debugOpt.Binary,
		ScriptsDir: m.debugOpt.ScriptsDir,
		Interface:  cfg.Interface,
		Target:     cfg.Target,
		Transport:  "swd",
		Serial:     probeSerial(desc.ID),
		SpeedKHz:   cfg.SpeedKHz,
		BindTo:     m.debugOpt.BindTo,
		GDBPort:    port,
	}
	if _, err := c.Args(); err != nil {
		return ocd.Config{}, err
	}
	return c, nil
}

// StartDebug hands a board's probe to OpenOCD and leaves it there, serving the
// GDB remote protocol on a TCP port until StopDebug is called or OpenOCD exits.
//
// The board is claimed for the whole session. Discovery keeps observing it —
// presence and port stay live in the UI — but will not attach a console to it,
// which is exactly what "someone else owns this board" should mean.
//
// ctx bounds only the wait for OpenOCD to come up; the session outlives it.
func (m *Manager) StartDebug(ctx context.Context, id string) error {
	if err := ocd.Available(m.debugOpt.Binary); err != nil {
		return err
	}
	md, desc, err := m.claim(id, busyDebug, true)
	if err != nil {
		return err
	}
	// From here every failure path must release the claim.
	fail := func(err error) error {
		md.mu.Lock()
		md.lastErr = err.Error()
		md.mu.Unlock()
		m.release(md)
		return err
	}

	if md.debugServer() != nil {
		return fail(fmt.Errorf("device %q already has a debug session", id))
	}
	cfg, err := m.DebugConfigFor(id)
	if err != nil {
		return fail(err)
	}
	port, err := ocd.FreePort(m.debugOpt.PortBase)
	if err != nil {
		return fail(err)
	}
	oc, err := m.ocdConfig(desc, cfg, port)
	if err != nil {
		return fail(err)
	}

	// OpenOCD resets and halts the chip, so let go of the console first. This
	// is the same single detach path everything else uses.
	m.detachLocked(md)

	// The ring is installed before the launch so a failed start still leaves
	// OpenOCD's own explanation on the page.
	ring := newRingBuffer(debugRingSize)
	md.mu.Lock()
	md.ocdRing = ring
	md.mu.Unlock()

	srv, err := ocd.Start(ctx, oc, &debugWriter{m: m, id: id, ring: ring})
	if err != nil {
		return fail(err)
	}

	md.mu.Lock()
	md.ocdSrv = srv
	md.mu.Unlock()
	slog.Info("debug session started", "device", id, "addr", srv.Addr(), "target", oc.Target, "interface", oc.Interface)
	m.hub.Publish(sseMessage{Event: eventDevices})

	// The claim is deliberately not released here: it is released when the
	// session ends. Go's sync.Mutex is not owner-tracked, so unlocking ctrl
	// from the watcher goroutine is well defined.
	go m.watchDebug(md, srv)
	return nil
}

// watchDebug is the single teardown path for a debug session, so a crashed
// OpenOCD and a clicked Stop end the same way.
func (m *Manager) watchDebug(md *managed, srv *ocd.Server) {
	<-srv.Done()
	md.mu.Lock()
	if md.ocdSrv == srv {
		md.ocdSrv = nil
	}
	id := md.desc.ID
	if err := srv.Err(); err != nil {
		md.lastErr = err.Error()
	}
	md.mu.Unlock()
	slog.Info("debug session ended", "device", id, "err", srv.Err())
	m.release(md)
}

// StopDebug ends a device's debug session. The board re-attaches on the next
// reconcile pass unless policy says otherwise.
func (m *Manager) StopDebug(id string) error {
	md := m.get(id, false)
	if md == nil {
		return fmt.Errorf("unknown device %q", id)
	}
	srv := md.debugServer()
	if srv == nil {
		return fmt.Errorf("device %q has no debug session", id)
	}
	return srv.Stop()
}

// debugServer returns the device's running OpenOCD session, or nil.
func (md *managed) debugServer() *ocd.Server {
	md.mu.Lock()
	defer md.mu.Unlock()
	return md.ocdSrv
}

// debugWriter fans OpenOCD's output out to the device's tail buffer and to the
// live page, mirroring what the console pump does for serial output.
type debugWriter struct {
	m    *Manager
	id   string
	ring *ringBuffer
}

func (w *debugWriter) Write(p []byte) (int, error) {
	w.ring.Write(p)
	if text := strings.TrimRight(string(p), "\r\n"); text != "" {
		w.m.hub.Publish(sseMessage{
			Event: debugEvent(w.id),
			Data:  "<div>" + html.EscapeString(text) + "</div>",
		})
	}
	return len(p), nil
}

// DebugPageView is everything the per-device debug page renders: the probe, how
// OpenOCD will be configured for it, and how to reach a running session.
type DebugPageView struct {
	Device         DeviceView
	Config         DebugConfig // effective config: stored settings over defaults
	ConfigErr      string      // why no usable config could be derived
	OpenOCDErr     string      // why OpenOCD cannot be run here, empty when it can
	OpenOCDPath    string      // the executable that will actually be run
	OpenOCDScripts string      // the config search path it will be given, if any
	OpenOCDVersion string
	// TargetOverride is the pinned board family, or flash.TargetUnknown when
	// the family is whatever discovery inferred.
	TargetOverride flash.Target
	Bind           string // the -ocd-bind address, for the exposure note
	GDBHost        string // host the browser reached picohub on
	GDBAddr        string // host:port a remote gdb connects to, while running
	Running        bool
}

// Exposed reports whether a session's port is reachable from the network
// rather than from this host only.
func (v DebugPageView) Exposed() bool {
	ip := net.ParseIP(v.Bind)
	return ip == nil || !ip.IsLoopback()
}

// GDBCommand is the command to run on the developer's machine against a
// running session.
func (v DebugPageView) GDBCommand() string {
	return "gdb-multiarch out.elf \\\n" +
		"  -ex \"target extended-remote " + v.GDBAddr + "\" \\\n" +
		"  -ex \"monitor halt\" \\\n" +
		"  -ex \"load\" \\\n" +
		"  -ex \"monitor reset halt\""
}

// DebugBindAddr is the address debug sessions are configured to listen on.
func (m *Manager) DebugBindAddr() string { return m.debugOpt.BindTo }

// DebugToolchain reports where OpenOCD was found and which config search path
// it will be given, or why it could not be found. The page shows this because
// "not on PATH" is a question about this process's environment, not the
// operator's shell.
func (m *Manager) DebugToolchain() (exe, scripts, version string, err error) {
	exe, err = ocd.Resolve(m.debugOpt.Binary)
	if err != nil {
		return "", "", "", err
	}
	scripts = ocd.ScriptsFor(m.debugOpt.Binary, m.debugOpt.ScriptsDir)
	version, _ = ocd.Version(m.debugOpt.Binary)
	return exe, scripts, version, nil
}
