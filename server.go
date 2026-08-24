package main

import (
	"cmp"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/a-h/templ"
	"github.com/soypat/picohub/flash"
)

// Server wires the Manager, Store, and SSE Hub to HTTP routes.
type Server struct {
	store *Store
	mgr   *Manager
	hub   *Hub
}

func NewServer(store *Store, mgr *Manager, hub *Hub) *Server {
	return &Server{store: store, mgr: mgr, hub: hub}
}

// Handler builds the route mux.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /", s.handleDashboard)
	mux.HandleFunc("GET /partials/devices", s.handleDeviceListPartial)
	mux.HandleFunc("GET /partials/devices/{id}/status", s.handleDeviceStatusPartial)
	mux.HandleFunc("GET /devices/{id}", s.handleDevice)
	mux.HandleFunc("POST /devices/{id}/flash", s.handleFlash)
	mux.HandleFunc("POST /devices/{id}/bootmode", s.handleBootMode)
	mux.HandleFunc("POST /devices/{id}/send", s.handleSend)
	mux.HandleFunc("POST /devices/{id}/rename", s.handleRename)
	mux.HandleFunc("POST /devices/{id}/ignore", s.handleIgnore)
	mux.HandleFunc("POST /devices/{id}/target", s.handleTarget)
	mux.HandleFunc("GET /devices/{id}/debug", s.handleDebugPage)
	mux.HandleFunc("POST /devices/{id}/debug/config", s.handleDebugConfig)
	mux.HandleFunc("POST /devices/{id}/debug/start", s.handleDebugStart)
	mux.HandleFunc("POST /devices/{id}/debug/stop", s.handleDebugStop)
	mux.HandleFunc("GET /devices/{id}/logs/{sid}", s.handleLog)
	mux.HandleFunc("GET /devices/{id}/logs/{sid}/raw", s.handleLogRaw)
	mux.HandleFunc("DELETE /devices/{id}/logs/{sid}", s.handleLogDelete)
	mux.HandleFunc("DELETE /devices/{id}/logs", s.handleLogsDeleteAll)
	mux.HandleFunc("GET /events", s.handleEvents)
	return mux
}

// --- rendering helpers -----------------------------------------------------

func (s *Server) render(w http.ResponseWriter, r *http.Request, title string, body templ.Component) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := page(title, body).Render(r.Context(), w); err != nil {
		slog.Error("render failed", "err", err)
	}
}

func (s *Server) renderPartial(w http.ResponseWriter, r *http.Request, c templ.Component) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := c.Render(r.Context(), w); err != nil {
		slog.Error("render partial failed", "err", err)
	}
}

// --- page handlers ---------------------------------------------------------

func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, "Devices", dashboard(s.mgr.Views()))
}

func (s *Server) handleDeviceListPartial(w http.ResponseWriter, r *http.Request) {
	s.renderPartial(w, r, deviceList(s.mgr.Views()))
}

func (s *Server) handleDeviceStatusPartial(w http.ResponseWriter, r *http.Request) {
	d, ok := s.mgr.View(r.PathValue("id"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	s.renderPartial(w, r, deviceStatus(d))
}

func (s *Server) handleDevice(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	d, ok := s.mgr.View(id)
	if !ok {
		http.NotFound(w, r)
		return
	}
	sessions, _ := s.store.Sessions(id)
	// The active session's byte count is only persisted when it ends, so the
	// stored copy reads 0 while live. Overlay the manager's live count.
	for i := range sessions {
		if sessions[i].ID == d.Session.ID {
			sessions[i].ByteLen = d.Session.ByteLen
		}
	}
	flashes, _ := s.store.Flashes(id)
	s.render(w, r, deviceName(d), devicePage(d, sessions, flashes))
}

func (s *Server) handleLog(w http.ResponseWriter, r *http.Request) {
	id, sid := r.PathValue("id"), r.PathValue("sid")
	d, ok := s.mgr.View(id)
	if !ok {
		http.NotFound(w, r)
		return
	}
	sess, found, _ := s.store.Session(sid)
	if !found || sess.DeviceID != id {
		http.NotFound(w, r)
		return
	}
	if sess.ID == d.Session.ID {
		sess.ByteLen = d.Session.ByteLen // live count; store lags until the session ends
	}
	content, _ := os.ReadFile(sess.LogFile)
	s.render(w, r, "log", logPage(d, sess, string(content)))
}

func (s *Server) handleLogRaw(w http.ResponseWriter, r *http.Request) {
	id, sid := r.PathValue("id"), r.PathValue("sid")
	sess, found, _ := s.store.Session(sid)
	if !found || sess.DeviceID != id {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", logFileName(id, sess)))
	http.ServeFile(w, r, sess.LogFile)
}

func (s *Server) handleLogDelete(w http.ResponseWriter, r *http.Request) {
	id, sid := r.PathValue("id"), r.PathValue("sid")
	sess, found, _ := s.store.Session(sid)
	if !found || sess.DeviceID != id {
		http.NotFound(w, r)
		return
	}
	// The live session is owned by the pump, which holds its file handle open;
	// refuse to delete it until monitoring ends.
	if sess.Active() {
		http.Error(w, "cannot delete a live session", http.StatusConflict)
		return
	}
	if _, err := s.store.DeleteSession(sid); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// The log view page asks to be redirected back to the device once its
	// session is gone; the session table just drops the row in place.
	if r.URL.Query().Get("redirect") != "" {
		w.Header().Set("HX-Redirect", "/devices/"+id)
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleLogsDeleteAll(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := s.store.DeleteSessions(id); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// Re-render the session table in place; a live session (if any) survives.
	sessions, _ := s.store.Sessions(id)
	s.renderPartial(w, r, sessionTable(id, sessions))
}

// --- action handlers -------------------------------------------------------

func (s *Server) handleFlash(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	d, ok := s.mgr.View(id)
	if !ok {
		http.Error(w, "unknown device", http.StatusNotFound)
		return
	}
	if d.Target.IsESP() {
		http.Error(w, "ESP flashing not supported yet", http.StatusNotImplemented)
		return
	}

	if err := r.ParseMultipartForm(32 << 20); err != nil {
		http.Error(w, "bad form: "+err.Error(), http.StatusBadRequest)
		return
	}
	file, hdr, err := r.FormFile("firmware")
	if err != nil {
		http.Error(w, "missing firmware file", http.StatusBadRequest)
		return
	}
	defer file.Close()

	tmp, err := os.CreateTemp("", "picohub-fw-*")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	tmpName := tmp.Name()
	if _, err := io.Copy(tmp, file); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	tmp.Close()

	// An ELF is converted to a UF2 for this board's target; a .uf2 is used as
	// it arrived. What the upload actually is decides, not its filename.
	fwPath, cleanup, err := flash.PrepareFirmware(tmpName, d.Target)
	if err != nil {
		os.Remove(tmpName)
		http.Error(w, "unusable firmware: "+err.Error(), http.StatusBadRequest)
		return
	}

	// Flash asynchronously; progress + result stream over SSE. The history
	// records the name the user uploaded, not the converted temporary.
	go func() {
		defer os.Remove(tmpName)
		defer cleanup()
		if err := s.mgr.Flash(context.Background(), id, fwPath, hdr.Filename); err != nil {
			slog.Error("flash failed", "device", id, "err", err)
		}
	}()
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleBootMode(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.mgr.EnterBootMode(r.Context(), id); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleSend(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.mgr.Send(id, r.FormValue("line")); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleRename(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.store.SetDeviceName(id, r.FormValue("name")); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.hub.Publish(sseMessage{Event: eventDevices})
	w.WriteHeader(http.StatusNoContent)
}

// handleIgnore marks a board as ignored — picohub releases the port and leaves
// it alone for openocd and friends — or reclaims it. The manager publishes the
// device event itself, and the page re-renders from that.
func (s *Server) handleIgnore(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	ignored := r.FormValue("ignored") == "1"
	if err := s.mgr.SetIgnored(id, ignored); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// The device page swaps its controls on this flag, so reload it rather than
	// leaving stale buttons behind.
	w.Header().Set("HX-Refresh", "true")
	w.WriteHeader(http.StatusNoContent)
}

// handleTarget pins a board's family when discovery cannot work it out from
// VID/PID. An empty value clears the pin.
func (s *Server) handleTarget(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(r.FormValue("target"))
	target, ok := flash.ParseTarget(name)
	if !ok {
		http.Error(w, fmt.Sprintf("unknown board family %q", name), http.StatusBadRequest)
		return
	}
	if err := s.mgr.SetTargetOverride(r.PathValue("id"), target); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// The device and debug pages both key off the family, so reload.
	w.Header().Set("HX-Refresh", "true")
	w.WriteHeader(http.StatusNoContent)
}

// --- debug handlers --------------------------------------------------------

// handleDebugPage renders everything needed to debug a board from another
// machine: what the probe is, how OpenOCD will be configured for it, and the
// gdb command to paste once a session is running.
func (s *Server) handleDebugPage(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	d, ok := s.mgr.View(id)
	if !ok {
		http.NotFound(w, r)
		return
	}
	v := DebugPageView{
		Device:  d,
		Bind:    s.mgr.DebugBindAddr(),
		GDBHost: requestHost(r),
	}
	cfg, err := s.mgr.DebugConfigFor(id)
	if err != nil {
		v.ConfigErr = err.Error()
	}
	v.Config = cfg
	if rec, found, err := s.store.Device(id); err == nil && found {
		v.TargetOverride = rec.TargetOverride
	}
	exe, scripts, version, err := s.mgr.DebugToolchain()
	if err != nil {
		v.OpenOCDErr = err.Error()
	} else {
		v.OpenOCDPath, v.OpenOCDScripts, v.OpenOCDVersion = exe, scripts, version
	}
	v.Running = d.DebugAddr != ""
	if v.Running {
		if _, port, err := net.SplitHostPort(d.DebugAddr); err == nil {
			v.GDBAddr = net.JoinHostPort(v.GDBHost, port)
		}
	}
	s.render(w, r, "Debug "+deviceName(d), debugPage(v))
}

// handleDebugConfig persists the OpenOCD configuration for a probe. The
// manager validates it, so a bad name is a 400 here rather than a failed start
// later.
func (s *Server) handleDebugConfig(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	speed, err := strconv.Atoi(strings.TrimSpace(cmp.Or(r.FormValue("speed_khz"), "0")))
	if err != nil || speed < 0 {
		http.Error(w, "adapter speed must be a non-negative number of kHz", http.StatusBadRequest)
		return
	}
	cfg := DebugConfig{
		Interface: strings.TrimSpace(r.FormValue("interface")),
		Target:    strings.TrimSpace(r.FormValue("target")),
		SpeedKHz:  speed,
	}
	if err := s.mgr.SetDebugConfig(id, cfg); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.Header().Set("HX-Refresh", "true")
	w.WriteHeader(http.StatusNoContent)
}

// handleDebugStart hands the probe to OpenOCD. The request context is not used
// for the session: it bounds only the wait for OpenOCD to come up, and the
// session outlives the request.
func (s *Server) handleDebugStart(w http.ResponseWriter, r *http.Request) {
	if err := s.mgr.StartDebug(r.Context(), r.PathValue("id")); err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	w.Header().Set("HX-Refresh", "true")
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleDebugStop(w http.ResponseWriter, r *http.Request) {
	if err := s.mgr.StopDebug(r.PathValue("id")); err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	w.Header().Set("HX-Refresh", "true")
	w.WriteHeader(http.StatusNoContent)
}

// requestHost is the host the client used to reach picohub, which is the one a
// remote gdb should be pointed at. It falls back to the wildcard so the
// rendered command is still obviously a template rather than wrong.
func requestHost(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.Host)
	if err != nil {
		host = r.Host
	}
	if host == "" {
		host = "<picohub-host>"
	}
	return host
}

// --- SSE handlers ----------------------------------------------------------

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	s.hub.Serve(w, r)
}
