// Command picohub is an HTTP server that manages locally-connected USB boards
// (Raspberry Pi Pico / Pico2, and ESP32c3/s3 in a later phase). It continuously
// captures each board's serial output to a durable log and exposes a web UI to
// reflash a board and browse its logs.
//
//go:generate go tool templ generate
package main

import (
	"context"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	var (
		addr     = flag.String("addr", ":8081", "HTTP listen address")
		dbPath   = flag.String("db", "picohub.db", "path to the bbolt database file")
		logsDir  = flag.String("logs", "picohub-logs", "directory for per-session serial logs")
		interval = flag.Duration("poll", time.Second, "USB discovery poll interval")
		ringKB   = flag.Int("ring", 64, "in-memory console tail size per device, in KiB")
		debug    = flag.Bool("debug", false, "enable debug logging")
	)
	flag.Parse()

	level := slog.LevelInfo
	if *debug {
		level = slog.LevelDebug
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: level})))

	store, err := OpenStore(*dbPath, *logsDir)
	if err != nil {
		slog.Error("open store", "err", err)
		os.Exit(1)
	}
	defer store.Close()

	hub := NewHub()
	mgr := NewManager(store, hub, *interval, *ringKB*1024)
	srv := NewServer(store, mgr, hub)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go mgr.Run(ctx)

	httpSrv := &http.Server{Addr: *addr, Handler: srv.Handler()}
	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shutCtx)
	}()

	slog.Info("picohub listening", "addr", *addr, "db", *dbPath, "logs", *logsDir)
	if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		slog.Error("http server", "err", err)
		os.Exit(1)
	}
}
