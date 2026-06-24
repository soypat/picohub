package flash

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"
)

// consoleBaud is the bitrate used for the USB-CDC console. USB-CDC ignores the
// baud rate, but the library requires a valid mode.
const consoleBaud = 115200

// consoleReadTimeout bounds each console Read so callers can poll for shutdown.
const consoleReadTimeout = 300 * time.Millisecond

// ErrUnsupported is returned by Device methods that do not apply to a given
// board family (e.g. MountDir on an ESP board, which has no mass-storage mode).
var ErrUnsupported = errors.New("operation not supported by this device")

// Descriptor is the stable identity plus the live state of a managed board.
// The ID is stable across reboots and port renumbering; Port is not.
type Descriptor struct {
	ID       string    // stable id: USB serial number, else a by-path link
	Target   Target    // resolved board family
	Port     string    // current /dev/ttyACMx | /dev/ttyUSBx (may change on reboot)
	VID      uint16    // USB vendor id,  e.g. 0x2e8a (Raspberry Pi)
	PID      uint16    // USB product id, e.g. 0x0005
	Name     string    // user-assigned label (from Store), may be empty
	Present  bool      // currently enumerated on the bus
	BootSel  bool      // enumerated in RP2 BOOTSEL mass-storage mode (no console)
	LastSeen time.Time // last time the device was observed present
}

// VIDPID returns the USB id formatted as "vid:pid", e.g. "2e8a:0005".
func (d Descriptor) VIDPID() string {
	return fmt.Sprintf("%04x:%04x", d.VID, d.PID)
}

// Device is the abstraction over a single physical board. A single goroutine
// (the caller's console pump) owns Read; Write may be called to send input.
// EnterBootMode/Flash/MountDir are flashing-lifecycle operations and must not
// run concurrently with the console pump — the caller serializes them.
type Device interface {
	// Descriptor returns the board's identity and current state.
	Descriptor() Descriptor

	// ReadWriteCloser is the serial console: Read returns bytes printed by the
	// board, Write sends input to it, Close releases the underlying port.
	io.ReadWriteCloser

	// EnterBootMode puts the board into its programming mode:
	//   RP2: 1200-baud touch with DTR dropped -> BOOTSEL mass storage.
	//   ESP: DTR/RTS reset into the serial download mode.
	EnterBootMode(ctx context.Context) error

	// Flash writes firmware to the board (entering boot mode as needed) and
	// reboots it. fw is a .uf2 for RP2 boards, a .bin for ESP boards. progress
	// may be nil; when non-nil it is called with bytes written and total.
	Flash(ctx context.Context, fw io.Reader, size int64, progress func(done, total int64)) error

	// MountDir mounts the board's mass-storage volume and returns its
	// filesystem path plus a function to unmount it. RP2 only; ESP boards
	// return ErrUnsupported.
	MountDir(ctx context.Context) (path string, unmount func() error, err error)
}

// NewDevice builds the concrete Device for a descriptor's target.
func NewDevice(desc Descriptor) Device {
	if desc.Target.IsESP() {
		return newESPDevice(desc)
	}
	return newRP2Device(desc)
}
