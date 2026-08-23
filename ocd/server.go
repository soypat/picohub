package ocd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	// startTimeout bounds how long we wait for OpenOCD to report a listening
	// GDB port. Attaching to a target that is held in reset can take a moment;
	// far longer than this means it is never coming up.
	startTimeout = 15 * time.Second

	// stopGrace is how long OpenOCD gets to shut down cleanly after SIGINT
	// before it is killed. It releases the USB interface on the way out, so
	// this is worth waiting for.
	stopGrace = 3 * time.Second

	// tailLines is how much of OpenOCD's output is kept to explain a failure.
	tailLines = 12
)

// gdbListening matches OpenOCD's own announcement of the port it bound, e.g.
//
//	Info : Listening on port 3333 for gdb connections
var gdbListening = regexp.MustCompile(`Listening on port (\d+) for gdb connections`)

// parseGDBPort returns the GDB port announced by an OpenOCD output line, and
// whether the line announced one.
func parseGDBPort(line string) (int, bool) {
	m := gdbListening.FindStringSubmatch(line)
	if m == nil {
		return 0, false
	}
	port, err := strconv.Atoi(m[1])
	if err != nil {
		return 0, false
	}
	return port, true
}

// Server is a running OpenOCD process serving one probe. It owns the process
// until Stop is called or the process exits on its own.
type Server struct {
	cfg  Config
	cmd  *exec.Cmd
	tail *tailWriter
	done chan struct{}

	mu       sync.Mutex
	port     int
	exitErr  error
	stopping bool
}

// Start launches OpenOCD and returns once it reports that the GDB port is
// listening. logw receives OpenOCD's output as it arrives and may be nil.
//
// ctx bounds only the wait for readiness: once Start returns, the session
// outlives ctx and is ended by Stop. That is deliberate, so a session can be
// started from an HTTP request without dying when the request ends.
func Start(ctx context.Context, cfg Config, logw io.Writer) (*Server, error) {
	exe, err := Resolve(cfg.Binary)
	if err != nil {
		return nil, err
	}
	// Derive the config search path here rather than in Args, which stays pure.
	cfg.ScriptsDir = ScriptsFor(cfg.Binary, cfg.ScriptsDir)
	args, err := cfg.Args()
	if err != nil {
		return nil, err
	}

	ready := make(chan int, 1)
	tail := &tailWriter{
		out: logw,
		onLine: func(line string) {
			if port, ok := parseGDBPort(line); ok {
				select {
				case ready <- port:
				default:
				}
			}
		},
	}

	cmd := exec.Command(exe, args...)
	cmd.Stdout = tail
	cmd.Stderr = tail
	// Give OpenOCD its own process group so Stop can signal anything it
	// spawned. Otherwise a surviving child keeps the output pipe open and
	// cmd.Wait never returns.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("%s: %w", exe, err)
	}

	s := &Server{cfg: cfg, cmd: cmd, tail: tail, done: make(chan struct{})}
	go func() {
		err := cmd.Wait()
		tail.Flush()
		s.mu.Lock()
		if err != nil && !s.stopping {
			s.exitErr = fmt.Errorf("openocd exited: %w: %s", err, tail.String())
		}
		s.mu.Unlock()
		close(s.done)
	}()

	select {
	case port := <-ready:
		s.mu.Lock()
		s.port = port
		s.mu.Unlock()
		return s, nil

	case <-s.done:
		// OpenOCD died before it listened. Its own last words are the useful
		// error: "unable to find a matching CMSIS-DAP device" and friends.
		return nil, fmt.Errorf("openocd did not start: %s", tail.String())

	case <-ctx.Done():
		_ = s.Stop()
		return nil, ctx.Err()

	case <-time.After(startTimeout):
		_ = s.Stop()
		return nil, fmt.Errorf("openocd did not report a listening gdb port within %s: %s", startTimeout, tail.String())
	}
}

// Config returns the configuration the session was started with.
func (s *Server) Config() Config { return s.cfg }

// Port is the GDB port OpenOCD actually bound, as it reported it.
func (s *Server) Port() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.port
}

// Addr is the address a remote gdb connects to: the configured bind address
// with the port OpenOCD reported it actually bound, so a mismatch between what
// was asked for and what happened shows up rather than being papered over.
func (s *Server) Addr() string {
	bind := s.cfg.BindTo
	if bind == "" {
		bind = "0.0.0.0"
	}
	return net.JoinHostPort(bind, strconv.Itoa(s.Port()))
}

// Done is closed when the OpenOCD process exits, however it exits.
func (s *Server) Done() <-chan struct{} { return s.done }

// Err reports why the session ended, or nil while it is running or after a
// requested Stop.
func (s *Server) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.exitErr
}

// Output returns the tail of OpenOCD's output.
func (s *Server) Output() string { return s.tail.String() }

// Stop ends the session. It is safe to call more than once, and safe to call
// on a session that has already exited.
func (s *Server) Stop() error {
	s.mu.Lock()
	if s.stopping {
		s.mu.Unlock()
		<-s.done
		return nil
	}
	s.stopping = true
	s.mu.Unlock()

	select {
	case <-s.done:
		return nil
	default:
	}

	// SIGINT first: OpenOCD releases the probe's USB interface on a clean
	// shutdown, and a killed process can leave it claimed.
	if err := s.signalGroup(syscall.SIGINT); err != nil {
		_ = s.signalGroup(syscall.SIGKILL)
	}
	select {
	case <-s.done:
		return nil
	case <-time.After(stopGrace):
	}
	if err := s.signalGroup(syscall.SIGKILL); err != nil {
		return fmt.Errorf("killing openocd: %w", err)
	}
	<-s.done
	return nil
}

// signalGroup signals OpenOCD and everything it spawned. A process that has
// already exited is not an error: Stop is allowed to race with a natural exit.
func (s *Server) signalGroup(sig syscall.Signal) error {
	pid := s.cmd.Process.Pid
	if err := syscall.Kill(-pid, sig); err != nil {
		if errors.Is(err, syscall.ESRCH) {
			return nil
		}
		// The group may not exist if setpgid did not take; fall back to the
		// process itself.
		if err := s.cmd.Process.Signal(sig); err != nil && !errors.Is(err, os.ErrProcessDone) {
			return err
		}
	}
	return nil
}

// tailWriter forwards OpenOCD's output to a writer while splitting it into
// lines for the readiness scan, and keeps the last few lines so a failure can
// be explained in OpenOCD's own words.
type tailWriter struct {
	out    io.Writer
	onLine func(line string)

	mu      sync.Mutex
	partial []byte
	lines   []string
}

func (t *tailWriter) Write(p []byte) (int, error) {
	if t.out != nil {
		_, _ = t.out.Write(p)
	}
	t.mu.Lock()
	t.partial = append(t.partial, p...)
	var complete []string
	for {
		i := bytes.IndexByte(t.partial, '\n')
		if i < 0 {
			break
		}
		line := strings.TrimRight(string(t.partial[:i]), "\r")
		t.partial = t.partial[i+1:]
		if line != "" {
			complete = append(complete, line)
			t.record(line)
		}
	}
	t.mu.Unlock()

	// Call out without the lock: onLine must never re-enter this writer.
	for _, line := range complete {
		t.onLine(line)
	}
	return len(p), nil
}

// Flush turns any buffered partial line into a complete one. OpenOCD's last
// message before exiting is often unterminated.
func (t *tailWriter) Flush() {
	t.mu.Lock()
	line := strings.TrimRight(string(t.partial), "\r\n")
	t.partial = nil
	if line != "" {
		t.record(line)
	}
	t.mu.Unlock()
}

// record appends a line to the tail. t.mu must be held.
func (t *tailWriter) record(line string) {
	t.lines = append(t.lines, line)
	if len(t.lines) > tailLines {
		t.lines = t.lines[len(t.lines)-tailLines:]
	}
}

func (t *tailWriter) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.lines) == 0 {
		return "(no output)"
	}
	return strings.Join(t.lines, "; ")
}

// firstLine collapses command output to its first non-empty line.
func firstLine(out []byte) string {
	for _, line := range strings.Split(string(out), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			return line
		}
	}
	return "(no output)"
}
