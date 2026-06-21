package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"
)

// ErrUnsupported is returned by Device methods that do not apply to a given
// board family (e.g. MountDir on an ESP board, which has no mass-storage mode).
var ErrUnsupported = errors.New("operation not supported by this device")

// Target identifies a board family and, implicitly, its flashing method.
type Target int

const (
	TargetUnknown Target = iota
	TargetPico           // RP2040: BOOTSEL mass-storage, .uf2
	TargetPico2          // RP2350: BOOTSEL mass-storage, .uf2
	TargetESP32C3        // espflasher over serial, .bin
	TargetESP32S3        // espflasher over serial, .bin
)

// IsRP2 reports whether the target flashes via the RP2 BOOTSEL mass-storage flow.
func (t Target) IsRP2() bool { return t == TargetPico || t == TargetPico2 }

// IsESP reports whether the target flashes via espflasher.
func (t Target) IsESP() bool { return t == TargetESP32C3 || t == TargetESP32S3 }

// FirmwareExt is the firmware file extension the target expects.
func (t Target) FirmwareExt() string {
	if t.IsRP2() {
		return ".uf2"
	}
	return ".bin"
}

func (t Target) String() string {
	switch t {
	case TargetPico:
		return "pico"
	case TargetPico2:
		return "pico2"
	case TargetESP32C3:
		return "esp32c3"
	case TargetESP32S3:
		return "esp32s3"
	default:
		return "unknown"
	}
}

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
	LastSeen time.Time // last time the device was observed present
}

// VIDPID returns the USB id formatted as "vid:pid", e.g. "2e8a:0005".
func (d Descriptor) VIDPID() string {
	return fmt.Sprintf("%04x:%04x", d.VID, d.PID)
}

// Device is the abstraction over a single physical board. A single goroutine
// (the Manager's console pump) owns Read; Write may be called to send input.
// EnterBootMode/Flash/MountDir are flashing-lifecycle operations and must not
// run concurrently with the console pump — the Manager serializes them.
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
