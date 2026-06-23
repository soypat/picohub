package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"html"
	"io"
	"log/slog"
	"os"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// DevState is the lifecycle state of a managed board.
type DevState int

const (
	StateAbsent     DevState = iota // not enumerated on the bus
	StateMonitoring                 // present, console pump running
	StateFlashing                   // a flash is in progress (board may be off-bus)
	StateBootsel                    // present in BOOTSEL mass-storage mode, ready to flash (no console)
)

func (s DevState) String() string {
	switch s {
	case StateMonitoring:
		return "monitoring"
	case StateFlashing:
		return "flashing"
	case StateBootsel:
		return "bootsel"
	default:
		return "absent"
	}
}

// managed is the Manager's per-board runtime state. Locking discipline:
//   - ctrl   serializes lifecycle transitions and is held across blocking waits
//     on pumpDone. The pump must never acquire ctrl or mu, so stopping it can
//     never deadlock.
//   - mu     guards the snapshot fields (state/desc/sess/lastErr) and is only
//     ever held briefly, never across a blocking operation.
//   - ring   is self-synchronized; logf is owned by the pump and closed only
//     after the pump has exited; byteLen is atomic.
type managed struct {
	ctrl sync.Mutex

	mu      sync.Mutex
	desc    Descriptor
	state   DevState
	sess    Session
	lastErr string

	dev      Device
	ring     *ringBuffer
	byteLen  atomic.Int64
	stop     chan struct{}
	pumpDone chan struct{}
}

// DeviceView is the read-only snapshot the HTTP layer renders.
type DeviceView struct {
	Descriptor
	State    DevState
	Session  Session
	LastErr  string
	RingText string
}

// Manager discovers boards, runs a console pump per board, and serializes
// flashing against the pump.
type Manager struct {
	store    *Store
	hub      *Hub
	disco    *discoverer
	interval time.Duration
	ringSize int

	mu      sync.Mutex
	devices map[string]*managed
}

func NewManager(store *Store, hub *Hub, interval time.Duration, ringSize int) *Manager {
	return &Manager{
		store:    store,
		hub:      hub,
		disco:    newDiscoverer(),
		interval: interval,
		ringSize: ringSize,
		devices:  make(map[string]*managed),
	}
}

// Run drives the discovery loop until ctx is cancelled, then shuts down pumps.
func (m *Manager) Run(ctx context.Context) {
	t := time.NewTicker(m.interval)
	defer t.Stop()
	m.reconcile(ctx)
	for {
		select {
		case <-ctx.Done():
			m.shutdown()
			return
		case <-t.C:
			m.reconcile(ctx)
		}
	}
}

// get returns the managed entry for id, creating an absent placeholder.
func (m *Manager) get(id string, create bool) *managed {
	m.mu.Lock()
	defer m.mu.Unlock()
	md := m.devices[id]
	if md == nil && create {
		md = &managed{state: StateAbsent}
		m.devices[id] = md
	}
	return md
}

// reconcile scans the bus and starts/stops monitoring to match what is present.
func (m *Manager) reconcile(ctx context.Context) {
	found, err := m.disco.scan()
	if err != nil {
		slog.Debug("discovery scan failed", "err", err)
		return
	}
	present := make(map[string]Descriptor, len(found))
	for _, d := range found {
		present[d.ID] = d
	}

	changed := false
	for id, desc := range present {
		md := m.get(id, true)
		switch md.snapshotState() {
		case StateFlashing:
			// Board legitimately drops off and returns during a flash; the
			// flash routine restores monitoring afterwards.
			continue
		case StateAbsent:
			ok := false
			if desc.BootSel {
				ok = m.registerBootsel(md, desc)
			} else {
				ok = m.startMonitoring(ctx, md, desc)
			}
			if ok {
				changed = true
			}
		default: // monitoring/bootsel: refresh the live port
			md.mu.Lock()
			md.desc.Port = desc.Port
			md.desc.LastSeen = time.Now()
			md.mu.Unlock()
		}
	}

	// Devices that vanished: stop the pump / clear bootsel and mark absent.
	m.mu.Lock()
	all := make([]*managed, 0, len(m.devices))
	ids := make([]string, 0, len(m.devices))
	for id, md := range m.devices {
		all = append(all, md)
		ids = append(ids, id)
	}
	m.mu.Unlock()
	for i, md := range all {
		if _, ok := present[ids[i]]; ok {
			continue
		}
		switch md.snapshotState() {
		case StateMonitoring:
			m.stopMonitoring(md)
			changed = true
		case StateBootsel:
			m.clearBootsel(md)
			changed = true
		}
	}

	if changed {
		m.hub.Publish(sseMessage{Event: eventDevices})
	}
}

func (md *managed) snapshotState() DevState {
	md.mu.Lock()
	defer md.mu.Unlock()
	return md.state
}

// startMonitoring opens the console, starts a session, and launches the pump.
func (m *Manager) startMonitoring(ctx context.Context, md *managed, desc Descriptor) bool {
	md.ctrl.Lock()
	defer md.ctrl.Unlock()
	if md.snapshotState() != StateAbsent {
		return false
	}

	rec, _ := m.store.DeviceSeen(desc.ID, time.Now())
	desc.Name = rec.Name
	if rec.TargetOverride != TargetUnknown {
		desc.Target = rec.TargetOverride
	}

	sess, err := m.store.SessionStart(desc.ID, time.Now())
	if err != nil {
		slog.Error("session start failed", "device", desc.ID, "err", err)
		return false
	}

	md.mu.Lock()
	md.desc = desc
	md.dev = newDevice(desc)
	md.sess = sess
	md.ring = newRingBuffer(m.ringSize)
	md.byteLen.Store(0)
	md.state = StateMonitoring
	md.lastErr = ""
	md.stop = make(chan struct{})
	md.pumpDone = make(chan struct{})
	dev := md.dev
	stop := md.stop
	done := md.pumpDone
	ring := md.ring
	md.mu.Unlock()

	go m.pump(md, dev, stop, done, ring, sess.LogFile, consoleEvent(desc.ID))
	slog.Info("monitoring", "device", desc.ID, "port", desc.Port, "target", desc.Target)
	return true
}

// registerBootsel records a board found in BOOTSEL mode as a present,
// flashable device. There is no console to pump, so no session is started.
func (m *Manager) registerBootsel(md *managed, desc Descriptor) bool {
	md.ctrl.Lock()
	defer md.ctrl.Unlock()
	if md.snapshotState() != StateAbsent {
		return false
	}

	rec, _ := m.store.DeviceSeen(desc.ID, time.Now())
	desc.Name = rec.Name
	if rec.TargetOverride != TargetUnknown {
		desc.Target = rec.TargetOverride
	}

	md.mu.Lock()
	md.desc = desc
	md.dev = newDevice(desc)
	md.state = StateBootsel
	md.lastErr = ""
	md.mu.Unlock()
	slog.Info("bootsel", "device", desc.ID, "target", desc.Target)
	return true
}

// clearBootsel marks a vanished BOOTSEL device absent (it rebooted into its
// firmware, or was unplugged).
func (m *Manager) clearBootsel(md *managed) {
	md.ctrl.Lock()
	defer md.ctrl.Unlock()
	md.mu.Lock()
	if md.state == StateBootsel {
		md.state = StateAbsent
		md.dev = nil
	}
	md.mu.Unlock()
	slog.Info("bootsel gone", "device", md.desc.ID)
}

// stopMonitoring stops the pump and ends the session (ctrl held across the wait).
func (m *Manager) stopMonitoring(md *managed) {
	md.ctrl.Lock()
	defer md.ctrl.Unlock()
	m.teardownLocked(md)
	md.mu.Lock()
	md.state = StateAbsent
	md.mu.Unlock()
	slog.Info("device gone", "device", md.desc.ID)
}

// teardownLocked stops the pump (if running), closes the device and log file,
// and records the session end. md.ctrl must be held; md.mu must NOT be.
func (m *Manager) teardownLocked(md *managed) {
	md.mu.Lock()
	st := md.state
	stop, done, dev, sessID := md.stop, md.pumpDone, md.dev, md.sess.ID
	md.mu.Unlock()

	if st != StateMonitoring {
		return
	}
	close(stop)
	<-done // safe: the pump acquires neither ctrl nor mu
	if dev != nil {
		_ = dev.Close()
	}
	if sessID != "" {
		byteLen := md.byteLen.Load()
		_ = m.store.SessionUpdate(sessID, func(s *Session) {
			s.EndedAt = time.Now()
			s.ByteLen = byteLen
		})
	}
}

// pump reads the console, appends to the log + ring, and broadcasts to SSE. It
// owns logf for the lifetime of the read loop and acquires no manager locks.
func (m *Manager) pump(md *managed, dev Device, stop, done chan struct{}, ring *ringBuffer, logFile, consoleEv string) {
	defer close(done)
	// Reopen the log file independently so the pump owns its handle.
	f, err := os.OpenFile(logFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		slog.Error("pump open log failed", "file", logFile, "err", err)
		return
	}
	defer f.Close()

	buf := make([]byte, 4096)
	for {
		select {
		case <-stop:
			return
		default:
		}
		n, rerr := dev.Read(buf)
		if n > 0 {
			chunk := buf[:n]
			_, _ = f.Write(chunk)
			ring.Write(chunk)
			md.byteLen.Add(int64(n))
			m.hub.Publish(sseMessage{Event: consoleEv, Data: html.EscapeString(string(chunk))})
		}
		if rerr != nil {
			slog.Debug("console read ended", "device", md.desc.ID, "err", rerr)
			return
		}
	}
}

// Flash serializes a flash against the pump: stop monitoring, end the session,
// run dev.Flash, record the result, then mark the device absent so the next
// reconcile re-establishes monitoring on the rebooted board.
func (m *Manager) Flash(ctx context.Context, id, fwPath, fwName string) error {
	md := m.get(id, false)
	if md == nil {
		return fmt.Errorf("unknown device %q", id)
	}
	md.ctrl.Lock()
	defer md.ctrl.Unlock()

	md.mu.Lock()
	state, dev := md.state, md.dev
	md.mu.Unlock()
	if state == StateFlashing {
		return fmt.Errorf("device %q is already flashing", id)
	}
	if dev == nil {
		return fmt.Errorf("device %q is not present", id)
	}

	m.teardownLocked(md)
	md.mu.Lock()
	md.state = StateFlashing
	md.lastErr = ""
	md.mu.Unlock()
	m.hub.Publish(sseMessage{Event: eventDevices})

	flashEv := flashEvent(id)
	rec := FlashRecord{DeviceID: id, At: time.Now(), Firmware: fwName}

	sum, size, err := fileSHA256(fwPath)
	if err == nil {
		rec.SHA256, rec.Size = sum, size
		var f *os.File
		f, err = os.Open(fwPath)
		if err == nil {
			defer f.Close()
			progress := func(done, total int64) {
				m.hub.Publish(sseMessage{Event: flashEv, Data: fmt.Sprintf("flashing: %d/%d bytes", done, total)})
			}
			m.hub.Publish(sseMessage{Event: flashEv, Data: "entering boot mode, flashing " + html.EscapeString(fwName) + " ..."})
			start := time.Now()
			err = dev.Flash(ctx, f, size, progress)
			rec.DurationMs = time.Since(start).Milliseconds()
		}
	}

	rec.OK = err == nil
	if err != nil {
		rec.Err = err.Error()
		m.hub.Publish(sseMessage{Event: flashEv, Data: "flash FAILED: " + html.EscapeString(err.Error())})
	} else {
		m.hub.Publish(sseMessage{Event: flashEv, Data: "flash OK; board rebooting"})
	}
	_ = m.store.FlashRecord(rec)

	md.mu.Lock()
	md.state = StateAbsent
	if err != nil {
		md.lastErr = err.Error()
	}
	md.mu.Unlock()
	m.hub.Publish(sseMessage{Event: eventDevices})
	return err
}

// EnterBootMode resets a present board into its programming mode without
// flashing (leaves an RP2 board in BOOTSEL with its volume available).
func (m *Manager) EnterBootMode(ctx context.Context, id string) error {
	md := m.get(id, false)
	if md == nil {
		return fmt.Errorf("unknown device %q", id)
	}
	md.ctrl.Lock()
	defer md.ctrl.Unlock()

	md.mu.Lock()
	dev := md.dev
	md.mu.Unlock()
	if dev == nil {
		return fmt.Errorf("device %q is not present", id)
	}
	m.teardownLocked(md)
	md.mu.Lock()
	md.state = StateAbsent
	md.mu.Unlock()
	m.hub.Publish(sseMessage{Event: eventDevices})
	return dev.EnterBootMode(ctx)
}

// Send writes a line (a trailing newline is added) to a board's console.
func (m *Manager) Send(id, line string) error {
	md := m.get(id, false)
	if md == nil {
		return fmt.Errorf("unknown device %q", id)
	}
	md.mu.Lock()
	dev := md.dev
	ok := md.state == StateMonitoring
	md.mu.Unlock()
	if !ok || dev == nil {
		return fmt.Errorf("device %q is not monitoring", id)
	}
	_, err := dev.Write([]byte(line + "\n"))
	return err
}

// View returns a snapshot of one device, or false if unknown.
func (m *Manager) View(id string) (DeviceView, bool) {
	md := m.get(id, false)
	if md == nil {
		return DeviceView{}, false
	}
	return md.view(), true
}

// Views returns snapshots of all known devices, sorted by ID.
func (m *Manager) Views() []DeviceView {
	m.mu.Lock()
	mds := make([]*managed, 0, len(m.devices))
	for _, md := range m.devices {
		mds = append(mds, md)
	}
	m.mu.Unlock()
	out := make([]DeviceView, 0, len(mds))
	for _, md := range mds {
		out = append(out, md.view())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func (md *managed) view() DeviceView {
	md.mu.Lock()
	defer md.mu.Unlock()
	v := DeviceView{
		Descriptor: md.desc,
		State:      md.state,
		Session:    md.sess,
		LastErr:    md.lastErr,
	}
	v.Session.ByteLen = md.byteLen.Load()
	v.Present = md.state != StateAbsent
	if md.ring != nil {
		v.RingText = md.ring.String()
	}
	return v
}

func (m *Manager) shutdown() {
	m.mu.Lock()
	mds := make([]*managed, 0, len(m.devices))
	for _, md := range m.devices {
		mds = append(mds, md)
	}
	m.mu.Unlock()
	for _, md := range mds {
		if md.snapshotState() == StateMonitoring {
			m.stopMonitoring(md)
		}
	}
}

// newDevice builds the concrete Device for a descriptor's target.
func newDevice(desc Descriptor) Device {
	if desc.Target.IsESP() {
		return newESPDevice(desc)
	}
	return newRP2Device(desc)
}

// fileSHA256 returns the hex SHA-256 and size of a file.
func fileSHA256(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}
