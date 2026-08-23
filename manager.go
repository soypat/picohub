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

	"github.com/soypat/picohub/flash"
	"github.com/soypat/picohub/ocd"
)

// A board's runtime state is four independent facts, not one enum. Keeping them
// apart is what lets policy ("leave this board alone") be expressed without
// touching the lifecycle code:
//
//   - presence — what the USB bus says. Reported by discovery, never decided here.
//   - attached — whether we hold the port and are pumping its console.
//   - ignored  — policy from the store: the user wants picohub to keep its hands off.
//   - busy     — an exclusive operation (flash, boot mode) owns the board.
//
// Only `attached` is ours to choose, and reconcile chooses it in exactly one
// place: wantAttached.

// Presence is what the USB bus says about a board.
type Presence int

const (
	Gone      Presence = iota // not enumerated on the bus
	OnConsole                 // enumerated as a USB-CDC tty
	OnBootsel                 // enumerated as RP2 BOOTSEL mass storage (no console)
)

func (p Presence) String() string {
	switch p {
	case OnConsole:
		return "console"
	case OnBootsel:
		return "bootsel"
	default:
		return "absent"
	}
}

// Names of the exclusive operations that can own a board, shown as-is in the UI.
const (
	busyFlashing = "flashing"
	busyBootMode = "entering boot mode"
	busyDebug    = "debugging"
)

// managed is the Manager's per-board runtime state. Locking discipline:
//   - ctrl   serializes attaching, detaching and exclusive operations, and is
//     held across blocking waits on pumpDone and for the whole of a flash. The
//     pump must never acquire ctrl or mu, so stopping it can never deadlock.
//     Discovery only ever TryLocks it, so a board someone else owns is skipped
//     rather than waited on.
//   - mu     guards the snapshot fields and is only ever held briefly, never
//     across a blocking operation.
//   - ring   is self-synchronized; the log file is owned by the pump and closed
//     only after the pump has exited; byteLen is atomic. The pump acquires mu
//     only briefly (via rollover) and never ctrl, so teardown — which waits on
//     pumpDone while holding ctrl but not mu — can never deadlock against it.
type managed struct {
	ctrl sync.Mutex

	mu       sync.Mutex
	desc     flash.Descriptor
	presence Presence
	attached bool   // console pump running and the port is ours
	ignored  bool   // mirrors DeviceRecord.Ignored
	busy     string // exclusive operation owning the board, "" when idle
	sess     Session
	lastErr  string

	dev      flash.Device
	ocdSrv   *ocd.Server // OpenOCD session owning the probe, nil when none
	ocdRing  *ringBuffer // OpenOCD output tail
	ring     *ringBuffer
	byteLen  atomic.Int64
	stop     chan struct{}
	pumpDone chan struct{}
}

// DeviceView is the read-only snapshot the HTTP layer renders. It exposes the
// four facts separately rather than a single collapsed status, so the UI can
// say "ignored and unplugged" without either fact hiding the other.
type DeviceView struct {
	flash.Descriptor
	Presence Presence
	Attached bool
	Ignored  bool
	Busy     string
	Session  Session
	LastErr  string
	RingText string

	// DebugAddr is the address a remote gdb connects to while a debug session
	// is running, and is empty when none is. DebugLog is that session's
	// OpenOCD output tail.
	DebugAddr string
	DebugLog  string
}

// Manager discovers boards, runs a console pump per attached board, and
// serializes exclusive operations against the pump.
type Manager struct {
	store    *Store
	hub      *Hub
	disco    *flash.Discoverer
	interval time.Duration
	ringSize int
	debugOpt DebugOptions
	poked    chan struct{} // wakes reconcile early after a policy change

	mu      sync.Mutex
	devices map[string]*managed
}

// DebugOptions says how debug sessions are exposed on the network.
type DebugOptions struct {
	// BindTo is the address OpenOCD's gdb port listens on. Empty means every
	// interface, which makes the port reachable from the network.
	BindTo string
	// PortBase is the first TCP port a session tries; each concurrent session
	// takes the next free one.
	PortBase int
	// Binary is the openocd executable: a name to look up on PATH, or a path.
	// Empty means "openocd". A service started by systemd does not inherit a
	// login shell's PATH, so a build outside the system prefixes needs this.
	Binary string
	// ScriptsDir is OpenOCD's config search path. Empty derives one from the
	// binary's location, see ocd.ScriptsFor.
	ScriptsDir string
}

func NewManager(store *Store, hub *Hub, interval time.Duration, ringSize int, debugOpt DebugOptions) *Manager {
	return &Manager{
		store:    store,
		hub:      hub,
		disco:    flash.NewDiscoverer(),
		interval: interval,
		ringSize: ringSize,
		debugOpt: debugOpt,
		poked:    make(chan struct{}, 1),
		devices:  make(map[string]*managed),
	}
}

// Run drives the discovery loop until ctx is cancelled, then shuts down pumps.
func (m *Manager) Run(ctx context.Context) {
	t := time.NewTicker(m.interval)
	defer t.Stop()
	m.reconcile()
	for {
		select {
		case <-ctx.Done():
			m.shutdown()
			return
		case <-t.C:
			m.reconcile()
		case <-m.poked:
			m.reconcile()
		}
	}
}

// poke asks for a reconcile pass now instead of at the next tick. It never
// blocks: a pass is already pending if the buffer is full.
func (m *Manager) poke() {
	select {
	case m.poked <- struct{}{}:
	default:
	}
}

// get returns the managed entry for id, creating an absent placeholder.
func (m *Manager) get(id string, create bool) *managed {
	m.mu.Lock()
	defer m.mu.Unlock()
	md := m.devices[id]
	if md == nil && create {
		// Seed the identity: a board can be created here before discovery has
		// ever described it (released while unplugged), and the UI links by ID.
		md = &managed{desc: flash.Descriptor{ID: id}}
		m.devices[id] = md
	}
	return md
}

// wantAttached reports whether picohub should be holding a board's console: it
// has to be on the bus as a CDC device, and not held back by policy. This is
// the only place the attach decision is made.
func wantAttached(desc flash.Descriptor, ignored bool) bool {
	return desc.Present && !desc.BootSel && !ignored
}

// applyRecord overlays a device's persisted, user-editable metadata onto a
// freshly discovered descriptor.
func applyRecord(desc flash.Descriptor, rec DeviceRecord) flash.Descriptor {
	desc.Name = rec.Name
	if rec.TargetOverride != flash.TargetUnknown {
		desc.Target = rec.TargetOverride
	}
	return desc
}

// reconcile makes the world match the bus and the stored policy: for every
// board it computes whether we want to be attached and converges to that. It
// does not branch on how the board got into its current state.
func (m *Manager) reconcile() {
	found, err := m.disco.Scan()
	if err != nil {
		slog.Debug("discovery scan failed", "err", err)
		return
	}
	onBus := make(map[string]flash.Descriptor, len(found))
	for _, d := range found {
		onBus[d.ID] = d
	}

	changed := false
	for _, id := range m.ids(onBus) {
		md := m.get(id, true)
		desc, present := onBus[id]
		ignored := md.isIgnored()
		if present {
			// While a board is on the bus the store is the authority on policy.
			rec := m.storedRecord(id)
			desc, ignored = applyRecord(desc, rec), rec.Ignored
		}
		if md.observe(desc, present, ignored) {
			changed = true
		}
		if m.converge(md, desc, wantAttached(desc, ignored)) {
			changed = true
		}
	}

	if changed {
		m.hub.Publish(sseMessage{Event: eventDevices})
	}
}

// ids returns every board worth a pass: the ones discovery just reported plus
// the ones we already track, so a board that vanished is noticed.
func (m *Manager) ids(onBus map[string]flash.Descriptor) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	ids := make([]string, 0, len(m.devices)+len(onBus))
	for id := range m.devices {
		ids = append(ids, id)
	}
	for id := range onBus {
		if _, known := m.devices[id]; !known {
			ids = append(ids, id)
		}
	}
	return ids
}

// storedRecord reads a board's persisted metadata, creating the record only the
// first time that board is ever seen. Reconcile runs every poll interval and a
// bolt write is an fsync, so the common path must stay a read; LastSeen is
// stamped on the transitions that matter instead (see attachLocked).
func (m *Manager) storedRecord(id string) DeviceRecord {
	rec, found, err := m.store.Device(id)
	if err == nil && found {
		return rec
	}
	rec, err = m.store.DeviceSeen(id, time.Now())
	if err != nil {
		slog.Debug("device record unavailable", "device", id, "err", err)
	}
	return rec
}

// observe records what discovery and the store say about a board. It opens and
// closes nothing — that is converge's job — and reports whether the board's
// presence changed, which is what the device list renders.
func (md *managed) observe(desc flash.Descriptor, present, ignored bool) bool {
	md.mu.Lock()
	defer md.mu.Unlock()
	was := md.presence
	md.ignored = ignored
	if !present {
		md.presence = Gone
		md.desc.Port = "" // the tty is gone; the board's identity is not
		return md.presence != was
	}
	desc.LastSeen = time.Now()
	md.desc = desc
	md.presence = OnConsole
	if desc.BootSel {
		md.presence = OnBootsel
	}
	return md.presence != was
}

// converge attaches or detaches a board to match want. It never waits for
// another operation to finish: a board someone else owns is left for the next
// pass, so discovery cannot be stalled by a slow flash.
func (m *Manager) converge(md *managed, desc flash.Descriptor, want bool) bool {
	if !md.ctrl.TryLock() {
		return false
	}
	defer md.ctrl.Unlock()
	switch {
	case want && !md.isAttached():
		return m.attachLocked(md, desc)
	case !want && md.isAttached():
		m.detachLocked(md)
		return true
	}
	return false
}

// attachLocked opens the console, starts a session, and launches the pump.
// md.ctrl must be held.
func (m *Manager) attachLocked(md *managed, desc flash.Descriptor) bool {
	if _, err := m.store.DeviceSeen(desc.ID, time.Now()); err != nil {
		slog.Debug("stamping LastSeen failed", "device", desc.ID, "err", err)
	}
	sess, err := m.store.SessionStart(desc.ID, time.Now())
	if err != nil {
		slog.Error("session start failed", "device", desc.ID, "err", err)
		return false
	}

	md.mu.Lock()
	md.desc = desc
	md.dev = flash.NewDevice(desc)
	md.sess = sess
	md.ring = newRingBuffer(m.ringSize)
	md.byteLen.Store(0)
	md.attached = true
	md.lastErr = ""
	md.stop = make(chan struct{})
	md.pumpDone = make(chan struct{})
	dev, stop, done, ring := md.dev, md.stop, md.pumpDone, md.ring
	md.mu.Unlock()

	go m.pump(md, dev, stop, done, ring, sess.LogFile, consoleEvent(desc.ID))
	slog.Info("attached", "device", desc.ID, "port", desc.Port, "target", desc.Target)
	return true
}

// detach releases a board: it stops the pump, closes the port and ends the live
// session. It is the only implementation of "let go of this board" — reconcile,
// Flash, EnterBootMode, SetIgnored and shutdown all route through it.
func (m *Manager) detach(md *managed) {
	md.ctrl.Lock()
	defer md.ctrl.Unlock()
	m.detachLocked(md)
}

// detachLocked is detach with md.ctrl already held; md.mu must NOT be.
func (m *Manager) detachLocked(md *managed) {
	md.mu.Lock()
	attached := md.attached
	stop, done, dev, sessID := md.stop, md.pumpDone, md.dev, md.sess.ID
	md.attached = false
	md.dev = nil
	md.mu.Unlock()

	if !attached {
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
	slog.Info("detached", "device", md.descID())
}

// claim takes a board for an exclusive operation, naming the activity for the
// UI. It returns the board's descriptor with md.ctrl held: the caller must call
// release when done. Discovery only TryLocks ctrl, so it leaves the board alone
// until then.
//
// allowIgnored admits operations that do not need the serial port. An ignored
// board is one picohub must not open a console on by itself; an explicitly
// requested debug session claims the probe's SWD interface instead, which is a
// different resource, so the policy does not apply to it.
func (m *Manager) claim(id, activity string, allowIgnored bool) (*managed, flash.Descriptor, error) {
	md := m.get(id, false)
	if md == nil {
		return nil, flash.Descriptor{}, fmt.Errorf("unknown device %q", id)
	}
	if !md.ctrl.TryLock() {
		return nil, flash.Descriptor{}, fmt.Errorf("device %q is busy", id)
	}

	md.mu.Lock()
	desc, presence, ignored := md.desc, md.presence, md.ignored
	md.mu.Unlock()

	var err error
	switch {
	case ignored && !allowIgnored:
		err = fmt.Errorf("device %q is ignored; resume picohub control first", id)
	case presence == Gone:
		err = fmt.Errorf("device %q is not present", id)
	}
	if err != nil {
		md.ctrl.Unlock()
		return nil, flash.Descriptor{}, err
	}

	md.mu.Lock()
	md.busy = activity
	md.lastErr = ""
	md.mu.Unlock()
	m.hub.Publish(sseMessage{Event: eventDevices})
	return md, desc, nil
}

// release ends an exclusive operation and asks reconcile to re-attach the board
// if policy says it should be attached.
func (m *Manager) release(md *managed) {
	md.mu.Lock()
	md.busy = ""
	md.mu.Unlock()
	md.ctrl.Unlock()
	m.hub.Publish(sseMessage{Event: eventDevices})
	m.poke()
}

func (md *managed) isAttached() bool {
	md.mu.Lock()
	defer md.mu.Unlock()
	return md.attached
}

func (md *managed) isIgnored() bool {
	md.mu.Lock()
	defer md.mu.Unlock()
	return md.ignored
}

func (md *managed) descID() string {
	md.mu.Lock()
	defer md.mu.Unlock()
	return md.desc.ID
}

// maxLogBytes is the size at which a session's log file rolls over into a
// continuation session, capping any single file.
const maxLogBytes = 5 << 20 // 5 MiB

// pump reads the console, appends to the log + ring, and broadcasts to SSE. It
// owns the log file for the lifetime of the read loop and acquires no manager
// locks except briefly via rollover.
func (m *Manager) pump(md *managed, dev flash.Device, stop, done chan struct{}, ring *ringBuffer, logFile, consoleEv string) {
	defer close(done)
	// Reopen the log file independently so the pump owns its handle.
	f, err := os.OpenFile(logFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		slog.Error("pump open log failed", "file", logFile, "err", err)
		return
	}
	defer func() { f.Close() }()

	var fileBytes int64
	if fi, err := f.Stat(); err == nil {
		fileBytes = fi.Size()
	}

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
			fileBytes += int64(n)
			m.hub.Publish(sseMessage{Event: consoleEv, Data: html.EscapeString(string(chunk))})
			if fileBytes >= maxLogBytes {
				newFile, ok := m.rollover(md)
				if ok {
					nf, err := os.OpenFile(newFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
					if err != nil {
						slog.Error("pump open rollover log failed", "file", newFile, "err", err)
						return
					}
					_ = f.Close()
					f, fileBytes = nf, 0
				}
			}
		}
		if rerr != nil {
			slog.Debug("console read ended", "device", md.descID(), "err", rerr)
			return
		}
	}
}

// rollover ends the device's current session and starts a continuation session
// for the same device, returning the log file the pump should append to next.
// Called by the pump when the active log file reaches maxLogBytes. The live
// console ring is intentionally left untouched so the on-screen tail stays
// continuous across the file boundary.
func (m *Manager) rollover(md *managed) (string, bool) {
	md.mu.Lock()
	prev := md.sess
	md.mu.Unlock()

	total := md.byteLen.Load()
	_ = m.store.SessionUpdate(prev.ID, func(s *Session) {
		s.EndedAt = time.Now()
		s.ByteLen = total
	})

	sess, err := m.store.SessionContinue(prev, time.Now())
	if err != nil {
		slog.Error("log rollover failed", "device", prev.DeviceID, "err", err)
		return "", false
	}

	md.mu.Lock()
	md.sess = sess
	md.mu.Unlock()
	md.byteLen.Store(0)

	slog.Info("log rollover", "device", prev.DeviceID, "seq", sess.Seq, "continues", prev.Seq)
	m.hub.Publish(sseMessage{Event: eventDevices})
	return sess.LogFile, true
}

// flashLog publishes one flash-status line wrapped in a block element, so the
// #flash-log container (which appends with beforeend) shows each on its own row
// rather than running them together inline.
func (m *Manager) flashLog(ev, text string) {
	m.hub.Publish(sseMessage{Event: ev, Data: "<div>" + text + "</div>"})
}

// Flash claims the board, drops the console so the port is free, writes the
// firmware, and records the result. Releasing the claim lets the next reconcile
// pass re-attach the rebooted board.
func (m *Manager) Flash(ctx context.Context, id, fwPath, fwName string) error {
	md, desc, err := m.claim(id, busyFlashing, false)
	if err != nil {
		return err
	}
	defer m.release(md)
	m.detachLocked(md)

	// A fresh Device: the one the pump was using has been closed with it.
	dev := flash.NewDevice(desc)
	defer dev.Close()

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
				m.flashLog(flashEv, fmt.Sprintf("flashing: %d/%d bytes", done, total))
			}
			m.flashLog(flashEv, "entering boot mode, flashing "+html.EscapeString(fwName)+" ...")
			start := time.Now()
			err = dev.Flash(ctx, f, size, progress)
			rec.DurationMs = time.Since(start).Milliseconds()
		}
	}

	rec.OK = err == nil
	if err != nil {
		rec.Err = err.Error()
		m.flashLog(flashEv, "flash FAILED: "+html.EscapeString(err.Error()))
	} else {
		m.flashLog(flashEv, "flash OK; board rebooting")
	}
	_ = m.store.FlashRecord(rec)

	if err != nil {
		md.mu.Lock()
		md.lastErr = err.Error()
		md.mu.Unlock()
	}
	return err
}

// EnterBootMode resets a present board into its programming mode without
// flashing (leaves an RP2 board in BOOTSEL with its volume available).
func (m *Manager) EnterBootMode(ctx context.Context, id string) error {
	md, desc, err := m.claim(id, busyBootMode, false)
	if err != nil {
		return err
	}
	defer m.release(md)
	m.detachLocked(md)

	dev := flash.NewDevice(desc)
	defer dev.Close()
	return dev.EnterBootMode(ctx)
}

// SetIgnored marks a board as ignored (or reclaims it). Ignoring detaches
// immediately, so the port is free for an external tool by the time this
// returns; reclaiming leaves the re-attach to the reconcile pass it pokes.
func (m *Manager) SetIgnored(id string, ignored bool) error {
	if err := m.store.SetDeviceIgnored(id, ignored); err != nil {
		return err
	}
	md := m.get(id, true) // a board may be released while unplugged
	md.mu.Lock()
	md.ignored = ignored
	md.mu.Unlock()
	if ignored {
		m.detach(md)
	}
	slog.Info("ignore flag changed", "device", id, "ignored", ignored)
	m.hub.Publish(sseMessage{Event: eventDevices})
	m.poke()
	return nil
}

// SetTargetOverride pins a board's family, for the boards discovery cannot
// classify from VID/PID alone. An RP2350-based debug probe is the case that
// forces this: it enumerates as 2e8a:000c exactly like the RP2040-based one, so
// nothing on the bus distinguishes them. flash.TargetUnknown clears the pin and
// goes back to what discovery says.
func (m *Manager) SetTargetOverride(id string, t flash.Target) error {
	if err := m.store.SetDeviceTarget(id, t); err != nil {
		return err
	}
	md := m.get(id, true) // a board may be classified while unplugged
	// Reconcile re-derives this from the record on its next pass over a board
	// that is on the bus; setting it here only saves the UI a poll interval.
	// Clearing the pin on an absent board keeps showing the old value until the
	// board is seen again, which is what every other field does too.
	md.mu.Lock()
	if t != flash.TargetUnknown {
		md.desc.Target = t
	}
	md.mu.Unlock()
	slog.Info("target override changed", "device", id, "target", t)
	m.hub.Publish(sseMessage{Event: eventDevices})
	m.poke()
	return nil
}

// Send writes a line (a trailing newline is added) to a board's console.
func (m *Manager) Send(id, line string) error {
	md := m.get(id, false)
	if md == nil {
		return fmt.Errorf("unknown device %q", id)
	}
	md.mu.Lock()
	dev, attached := md.dev, md.attached
	md.mu.Unlock()
	if !attached || dev == nil {
		return fmt.Errorf("device %q is not attached", id)
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
		Presence:   md.presence,
		Attached:   md.attached,
		Ignored:    md.ignored,
		Busy:       md.busy,
		Session:    md.sess,
		LastErr:    md.lastErr,
	}
	v.Session.ByteLen = md.byteLen.Load()
	v.Present = md.presence != Gone
	if md.ring != nil {
		v.RingText = md.ring.String()
	}
	if md.ocdRing != nil {
		v.DebugLog = md.ocdRing.String()
	}
	if md.ocdSrv != nil {
		v.DebugAddr = md.ocdSrv.Addr()
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
	// Stop debug sessions first: each holds md.ctrl, which detach needs, and
	// an orphaned OpenOCD would keep the probe claimed after we exit.
	for _, md := range mds {
		if srv := md.debugServer(); srv != nil {
			_ = srv.Stop()
		}
	}
	for _, md := range mds {
		m.detach(md)
	}
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
