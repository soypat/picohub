package ocd

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestConfigArgs(t *testing.T) {
	base := []string{
		"-c", "bindto 0.0.0.0",
		"-c", "gdb_port 3333",
		"-c", "telnet_port disabled",
		"-c", "tcl_port disabled",
		"-f", "interface/cmsis-dap.cfg",
	}
	tests := []struct {
		name string
		cfg  Config
		want []string
	}{
		{
			"minimal",
			Config{Interface: "cmsis-dap", Target: "rp2040"},
			append(slices.Clone(base), "-f", "target/rp2040.cfg"),
		},
		{
			"transport and speed",
			Config{Interface: "cmsis-dap", Target: "rp2350", Transport: "swd", SpeedKHz: 5000},
			append(slices.Clone(base),
				"-c", "transport select swd",
				"-c", "adapter speed 5000",
				"-f", "target/rp2350.cfg"),
		},
		{
			"serial selects one of several probes",
			Config{Interface: "cmsis-dap", Target: "rp2040", Serial: "E6614103E76A5D2F"},
			append(slices.Clone(base),
				"-c", "adapter serial E6614103E76A5D2F",
				"-f", "target/rp2040.cfg"),
		},
		{
			"explicit bind and port",
			Config{Interface: "picoprobe", Target: "rp2040", BindTo: "127.0.0.1", GDBPort: 3350},
			[]string{
				"-c", "bindto 127.0.0.1",
				"-c", "gdb_port 3350",
				"-c", "telnet_port disabled",
				"-c", "tcl_port disabled",
				"-f", "interface/picoprobe.cfg",
				"-f", "target/rp2040.cfg",
			},
		},
		{
			"extra commands come last",
			Config{Interface: "cmsis-dap", Target: "rp2040", Commands: []string{"init", "reset init"}},
			append(slices.Clone(base),
				"-f", "target/rp2040.cfg",
				"-c", "init",
				"-c", "reset init"),
		},
	}
	for _, tt := range tests {
		got, err := tt.cfg.Args()
		if err != nil {
			t.Errorf("%s: %v", tt.name, err)
			continue
		}
		if !slices.Equal(got, tt.want) {
			t.Errorf("%s:\n got %q\nwant %q", tt.name, got, tt.want)
		}
	}
}

// Names reach Args from a persisted web form and end up inside an OpenOCD
// config path or command string. They must be rejected, never sanitized.
func TestConfigArgsRejects(t *testing.T) {
	tests := []struct {
		name string
		cfg  Config
	}{
		{"empty interface", Config{Target: "rp2040"}},
		{"empty target", Config{Interface: "cmsis-dap"}},
		{"command injection via target", Config{Interface: "cmsis-dap", Target: "rp2040 -c shutdown"}},
		{"path traversal via interface", Config{Interface: "../../../etc/passwd", Target: "rp2040"}},
		{"path separator in target", Config{Interface: "cmsis-dap", Target: "a/b"}},
		{"quote in interface", Config{Interface: `a"b`, Target: "rp2040"}},
		{"newline in target", Config{Interface: "cmsis-dap", Target: "rp2040\nshutdown"}},
		{"semicolon in serial", Config{Interface: "cmsis-dap", Target: "rp2040", Serial: "x; reset"}},
		{"space in serial", Config{Interface: "cmsis-dap", Target: "rp2040", Serial: "a b"}},
		{"unsupported transport", Config{Interface: "cmsis-dap", Target: "rp2040", Transport: "jtag"}},
		{"bind is not an ip", Config{Interface: "cmsis-dap", Target: "rp2040", BindTo: "not-an-ip"}},
		{"bind is a hostname", Config{Interface: "cmsis-dap", Target: "rp2040", BindTo: "localhost"}},
		{"port out of range", Config{Interface: "cmsis-dap", Target: "rp2040", GDBPort: 70000}},
		{"negative speed", Config{Interface: "cmsis-dap", Target: "rp2040", SpeedKHz: -1}},
	}
	for _, tt := range tests {
		if args, err := tt.cfg.Args(); err == nil {
			t.Errorf("%s: accepted, produced %q", tt.name, args)
		}
	}
}

func TestParseGDBPort(t *testing.T) {
	tests := []struct {
		line string
		want int
		ok   bool
	}{
		{"Info : Listening on port 3333 for gdb connections", 3333, true},
		{"Info : Listening on port 3350 for gdb connections", 3350, true},
		{"Info : Listening on port 4444 for telnet connections", 0, false},
		{"Info : Listening on port 6666 for tcl connections", 0, false},
		{"Error: unable to find a matching CMSIS-DAP device", 0, false},
		{"", 0, false},
	}
	for _, tt := range tests {
		got, ok := parseGDBPort(tt.line)
		if ok != tt.ok || got != tt.want {
			t.Errorf("parseGDBPort(%q) = %d,%v want %d,%v", tt.line, got, ok, tt.want, tt.ok)
		}
	}
}

func TestConfigAddr(t *testing.T) {
	tests := []struct {
		cfg  Config
		want string
	}{
		{Config{}, "0.0.0.0:3333"},
		{Config{BindTo: "127.0.0.1", GDBPort: 3350}, "127.0.0.1:3350"},
		{Config{BindTo: "::1"}, "[::1]:3333"},
	}
	for _, tt := range tests {
		if got := tt.cfg.Addr(); got != tt.want {
			t.Errorf("Addr() = %q want %q", got, tt.want)
		}
	}
}

// tailWriter splits OpenOCD's output into lines regardless of how the writes
// happen to be chunked, and keeps the last of them to explain a failure.
func TestTailWriter(t *testing.T) {
	var seen []string
	tw := &tailWriter{onLine: func(l string) { seen = append(seen, l) }}
	tw.Write([]byte("Info : Listen"))
	tw.Write([]byte("ing on port 3333 for gdb connections\r\nErro"))
	tw.Write([]byte("r: boom\n"))
	if len(seen) != 2 || seen[0] != "Info : Listening on port 3333 for gdb connections" {
		t.Fatalf("lines not reassembled across writes: %q", seen)
	}
	tw.Write([]byte("unterminated last words"))
	tw.Flush()
	if got := tw.String(); got != "Info : Listening on port 3333 for gdb connections; Error: boom; unterminated last words" {
		t.Errorf("tail = %q", got)
	}
	if got := (&tailWriter{}).String(); got != "(no output)" {
		t.Errorf("empty tail = %q", got)
	}
}

// fakeOpenOCD writes an executable at dir/name and returns its path.
func fakeOpenOCD(t *testing.T, dir, name string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// A build that was never installed keeps its configs beside its binary, and
// must be told where they are. A system install must not be.
func TestScriptsFor(t *testing.T) {
	// Relocated build: openocd + scripts/interface/ side by side.
	reloc := t.TempDir()
	exe := fakeOpenOCD(t, reloc, "openocd")
	if err := os.MkdirAll(filepath.Join(reloc, "scripts", "interface"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got, want := ScriptsFor(exe, ""), filepath.Join(reloc, "scripts"); got != want {
		t.Errorf("relocated build: ScriptsFor = %q want %q", got, want)
	}

	// A build tree uses tcl/ rather than scripts/.
	tree := t.TempDir()
	treeExe := fakeOpenOCD(t, tree, "openocd")
	if err := os.MkdirAll(filepath.Join(tree, "tcl", "interface"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got, want := ScriptsFor(treeExe, ""), filepath.Join(tree, "tcl"); got != want {
		t.Errorf("build tree: ScriptsFor = %q want %q", got, want)
	}

	// System install: nothing beside the binary, so OpenOCD finds its own.
	plain := t.TempDir()
	plainExe := fakeOpenOCD(t, plain, "openocd")
	if got := ScriptsFor(plainExe, ""); got != "" {
		t.Errorf("system install should need no -s, got %q", got)
	}

	// An explicit setting always wins.
	if got := ScriptsFor(exe, "/somewhere/else"); got != "/somewhere/else" {
		t.Errorf("explicit dir ignored, got %q", got)
	}
	// An unresolvable binary must not panic or guess.
	if got := ScriptsFor(filepath.Join(reloc, "nope"), ""); got != "" {
		t.Errorf("unresolvable binary gave %q", got)
	}
}

func TestScriptsDirIsPassed(t *testing.T) {
	cfg := Config{Interface: "cmsis-dap", Target: "rp2350", ScriptsDir: "/opt/ocd/scripts"}
	args, err := cfg.Args()
	if err != nil {
		t.Fatal(err)
	}
	if len(args) < 2 || args[0] != "-s" || args[1] != "/opt/ocd/scripts" {
		t.Fatalf("-s must come first, got %q", args)
	}
	// Args is pure: with no ScriptsDir set, nothing is probed or guessed.
	cfg.ScriptsDir = ""
	args, _ = cfg.Args()
	if slices.Contains(args, "-s") {
		t.Errorf("Args added a search path on its own: %q", args)
	}
}

// The "not found" message has to say where it looked: the operator's shell PATH
// is not the one this process has.
func TestResolve(t *testing.T) {
	dir := t.TempDir()
	exe := fakeOpenOCD(t, dir, "openocd")
	if got, err := Resolve(exe); err != nil || got != exe {
		t.Errorf("Resolve(%q) = %q, %v", exe, got, err)
	}

	if _, err := Resolve(filepath.Join(dir, "missing")); err == nil {
		t.Error("a path that does not exist should not resolve")
	}
	if _, err := Resolve(dir); err == nil {
		t.Error("a directory should not resolve as the binary")
	}

	notExec := filepath.Join(dir, "openocd.txt")
	if err := os.WriteFile(notExec, []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Resolve(notExec); err == nil {
		t.Error("a non-executable file should not resolve")
	}

	t.Setenv("PATH", t.TempDir())
	err := Resolve1Err(t)
	if !strings.Contains(err.Error(), "PATH") || !strings.Contains(err.Error(), "-openocd") {
		t.Errorf("error should name PATH and the flag that fixes it, got %q", err)
	}

	t.Setenv("PATH", dir)
	if got, err := Resolve(""); err != nil || got != exe {
		t.Errorf("bare name should be looked up on PATH: %q, %v", got, err)
	}
}

func Resolve1Err(t *testing.T) error {
	t.Helper()
	_, err := Resolve("")
	if err == nil {
		t.Fatal("expected a lookup failure")
	}
	return err
}
