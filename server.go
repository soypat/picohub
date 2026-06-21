package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"

	"github.com/a-h/templ"
	"github.com/soypat/tinyboot/build/uf2"
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
	mux.HandleFunc("GET /devices/{id}/logs/{sid}", s.handleLog)
	mux.HandleFunc("GET /devices/{id}/logs/{sid}/raw", s.handleLogRaw)
	mux.HandleFunc("GET /events", s.handleEvents)
	mux.HandleFunc("GET /devices/{id}/console", s.handleConsole)
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
	http.ServeFile(w, r, sess.LogFile)
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

	tmp, err := os.CreateTemp("", "picohub-fw-*.uf2")
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

	if err := validateUF2(tmpName); err != nil {
		os.Remove(tmpName)
		http.Error(w, "invalid .uf2: "+err.Error(), http.StatusBadRequest)
		return
	}

	// Flash asynchronously; progress + result stream over SSE.
	go func() {
		defer os.Remove(tmpName)
		if err := s.mgr.Flash(context.Background(), id, tmpName, hdr.Filename); err != nil {
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
	s.hub.Publish(topicEvents, sseMessage{Event: "devices"})
	w.WriteHeader(http.StatusNoContent)
}

// --- SSE handlers ----------------------------------------------------------

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	s.hub.ServeTopic(w, r, topicEvents)
}

func (s *Server) handleConsole(w http.ResponseWriter, r *http.Request) {
	s.hub.ServeTopic(w, r, topicConsole(r.PathValue("id")))
}

// validateUF2 confirms the uploaded file decodes as a UF2 image.
func validateUF2(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	scratch := make([]byte, uf2.BlockSize)
	blocks, _, err := uf2.DecodeAppendBlocks(nil, f, scratch)
	if err != nil {
		return err
	}
	if len(blocks) == 0 {
		return fmt.Errorf("no UF2 blocks found")
	}
	return nil
}
