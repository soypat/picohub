// Package ocd supervises OpenOCD GDB servers so that a developer on another
// machine can debug a board attached to this host. It is independent of the
// picohub HTTP server so it can be driven directly from a CLI or tests.
//
// A session is one OpenOCD process serving one probe. OpenOCD speaks the GDB
// remote protocol on a TCP port; the ELF with its debug information stays on
// the developer's machine and never crosses the network, so this package does
// no binary parsing of any kind.
package ocd

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// DefaultGDBPort is the port OpenOCD serves the GDB remote protocol on when
// Config.GDBPort is zero. It is OpenOCD's own default.
const DefaultGDBPort = 3333

// DefaultInterface covers the Raspberry Pi Debug Probe and most DAPLink
// clones. A Pico running picoprobe firmware may need "picoprobe" instead,
// depending on the OpenOCD build.
const DefaultInterface = "cmsis-dap"

// cfgName is the character set OpenOCD configuration file names are allowed to
// use. Interface and Target are interpolated into a path handed to OpenOCD, so
// anything outside this set is rejected rather than escaped.
var cfgName = regexp.MustCompile(`^[\p{L}0-9_-]+$`)

// serialName is the character set an adapter serial may use. It is interpolated
// into an OpenOCD command string, so the same rule applies.
var serialName = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// Config is everything OpenOCD needs to serve one probe.
//
// Binary and ScriptsDir are operator settings and are paths; the rest describe
// the probe and reach this package from a web form, which is why only they are
// name-validated.
type Config struct {
	Binary     string   // openocd executable: a name on PATH or a path; empty means "openocd"
	ScriptsDir string   // OpenOCD config search path (-s); empty derives one, see ScriptsFor
	Interface  string   // interface cfg name, e.g. "cmsis-dap" or "picoprobe"
	Target     string   // target cfg name, e.g. "rp2040" or "rp2350"
	Transport  string   // "swd"; empty leaves OpenOCD's default
	Serial     string   // probe USB serial; picks one of several attached probes
	SpeedKHz   int      // adapter speed; 0 leaves OpenOCD's default
	BindTo     string   // listen address; empty means "0.0.0.0"
	GDBPort    int      // TCP port for the GDB remote protocol; 0 means 3333
	Commands   []string // extra -c commands, appended last
}

// Exe is the openocd executable the configuration runs.
func (c Config) Exe() string {
	if c.Binary == "" {
		return "openocd"
	}
	return c.Binary
}

// Port is the port the configuration asks OpenOCD to listen on.
func (c Config) Port() int {
	if c.GDBPort == 0 {
		return DefaultGDBPort
	}
	return c.GDBPort
}

// Addr is the address the configuration asks OpenOCD to listen on.
func (c Config) Addr() string {
	bind := c.BindTo
	if bind == "" {
		bind = "0.0.0.0"
	}
	return net.JoinHostPort(bind, strconv.Itoa(c.Port()))
}

// Args renders the OpenOCD command line. It is pure: ScriptsDir is used as
// given, so a caller that wants one derived calls ScriptsFor first (Start
// does).
//
// Order matters: the port settings must be issued before OpenOCD reaches its
// init stage, and "adapter serial"/"transport select"/"adapter speed" must come
// after the interface config that creates the adapter driver.
//
// Validation here is a security control, not a convenience. Interface and
// Target reach this function from a persisted web form and end up inside a
// config file path; Serial and BindTo end up inside OpenOCD command strings. A
// name that does not fit the expected shape is an error, never a sanitized
// pass-through.
func (c Config) Args() ([]string, error) {
	if !cfgName.MatchString(c.Interface) {
		return nil, fmt.Errorf("invalid OpenOCD interface name %q", c.Interface)
	}
	if !cfgName.MatchString(c.Target) {
		return nil, fmt.Errorf("invalid OpenOCD target name %q", c.Target)
	}
	if c.Serial != "" && !serialName.MatchString(c.Serial) {
		return nil, fmt.Errorf("invalid adapter serial %q", c.Serial)
	}
	if c.Transport != "" && c.Transport != "swd" {
		return nil, fmt.Errorf("unsupported OpenOCD transport %q", c.Transport)
	}
	if c.BindTo != "" && net.ParseIP(c.BindTo) == nil {
		return nil, fmt.Errorf("bind address %q is not an IP address", c.BindTo)
	}
	if c.GDBPort < 0 || c.GDBPort > 65535 {
		return nil, fmt.Errorf("gdb port %d out of range", c.GDBPort)
	}
	if c.SpeedKHz < 0 {
		return nil, fmt.Errorf("adapter speed %d out of range", c.SpeedKHz)
	}
	bind := c.BindTo
	if bind == "" {
		bind = "0.0.0.0"
	}

	// The telnet and Tcl RPC consoles expose the whole OpenOCD command
	// language, which is a far larger surface than the GDB port. They are
	// never opened.
	//
	// The underscore spellings are deprecated in OpenOCD's development builds,
	// which warn about them, but the spaced forms ("gdb port") do not exist in
	// 0.12.0 and earlier. Keep the underscores: a warning on new builds beats
	// an unknown command on released ones.
	var args []string
	if c.ScriptsDir != "" {
		// A relocated OpenOCD build does not know where its own configs live.
		args = append(args, "-s", c.ScriptsDir)
	}
	args = append(args,
		"-c", "bindto "+bind,
		"-c", "gdb_port "+strconv.Itoa(c.Port()),
		"-c", "telnet_port disabled",
		"-c", "tcl_port disabled",
		"-f", "interface/"+c.Interface+".cfg",
	)
	if c.Serial != "" {
		args = append(args, "-c", "adapter serial "+c.Serial)
	}
	if c.Transport != "" {
		args = append(args, "-c", "transport select "+c.Transport)
	}
	if c.SpeedKHz > 0 {
		args = append(args, "-c", "adapter speed "+strconv.Itoa(c.SpeedKHz))
	}
	args = append(args, "-f", "target/"+c.Target+".cfg")
	for _, cmd := range c.Commands {
		args = append(args, "-c", cmd)
	}
	return args, nil
}

// Resolve returns the absolute path of the openocd executable a configuration
// would run. The error says what was looked for and where, because "not on
// PATH" is useless when the process's PATH is not the one you typed in.
func Resolve(binary string) (string, error) {
	if binary == "" {
		binary = "openocd"
	}
	if strings.ContainsRune(binary, os.PathSeparator) {
		info, err := os.Stat(binary)
		if err != nil {
			return "", fmt.Errorf("openocd not found at %s: %w", binary, err)
		}
		if info.IsDir() || info.Mode()&0o111 == 0 {
			return "", fmt.Errorf("%s is not an executable file", binary)
		}
		return filepath.Abs(binary)
	}
	path, err := exec.LookPath(binary)
	if err != nil {
		return "", fmt.Errorf("%q not found on this process's PATH (%s); pass -openocd with the path to the binary",
			binary, os.Getenv("PATH"))
	}
	return path, nil
}

// ScriptsFor returns the OpenOCD config search path to pass as -s.
//
// An explicit dir wins. Otherwise, a build that carries its configs next to its
// binary — an unpacked release or a build tree that was never installed — is
// detected by looking for a scripts (or tcl) directory beside the executable.
// A system install has neither, and OpenOCD finds its own configs.
func ScriptsFor(binary, dir string) string {
	if dir != "" {
		return dir
	}
	exe, err := Resolve(binary)
	if err != nil {
		return ""
	}
	base := filepath.Dir(exe)
	for _, name := range []string{"scripts", "tcl"} {
		candidate := filepath.Join(base, name)
		if info, err := os.Stat(filepath.Join(candidate, "interface")); err == nil && info.IsDir() {
			return candidate
		}
	}
	return ""
}

// Available reports whether OpenOCD can be run at all, so a caller can say so
// up front instead of failing at launch.
func Available(binary string) error {
	_, err := Resolve(binary)
	return err
}

// Version returns the first line of "openocd --version", or an error if
// OpenOCD is unavailable.
func Version(binary string) (string, error) {
	exe, err := Resolve(binary)
	if err != nil {
		return "", err
	}
	// OpenOCD prints its version banner on stderr.
	out, err := exec.Command(exe, "--version").CombinedOutput()
	if err != nil && len(out) == 0 {
		return "", fmt.Errorf("%s --version: %w", exe, err)
	}
	return firstLine(out), nil
}

// FreePort returns a free TCP port at or above base, so several probes can be
// served at once. The port is closed before it is returned, so a caller that
// waits too long may lose the race; OpenOCD reports the port it actually bound
// and Server.Port is the authority.
func FreePort(base int) (int, error) {
	if base <= 0 {
		base = DefaultGDBPort
	}
	for port := base; port < base+64 && port <= 65535; port++ {
		ln, err := net.Listen("tcp", net.JoinHostPort("", strconv.Itoa(port)))
		if err != nil {
			continue
		}
		ln.Close()
		return port, nil
	}
	return 0, fmt.Errorf("no free TCP port in [%d,%d)", base, base+64)
}
