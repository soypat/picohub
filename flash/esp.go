package flash

import (
	"context"
	"fmt"
	"io"
	"sync"

	"go.bug.st/serial"
)

// espDevice is the ESP32c3/s3 implementation. Phase 1 implements only the
// serial console; flashing is wired up in Phase 2 using tinygo.org/x/espflasher.
// EnterBootMode/Flash currently return ErrUnsupported so the build and the
// Device interface are complete and ESP support is a drop-in addition.
type espDevice struct {
	desc Descriptor

	mu   sync.Mutex
	port serial.Port // console connection, lazily opened by Read/Write
}

func newESPDevice(desc Descriptor) *espDevice { return &espDevice{desc: desc} }

func (d *espDevice) Descriptor() Descriptor { return d.desc }

func (d *espDevice) openConsole() (serial.Port, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.port != nil {
		return d.port, nil
	}
	p, err := serial.Open(d.desc.Port, &serial.Mode{BaudRate: consoleBaud})
	if err != nil {
		return nil, fmt.Errorf("open console %s: %w", d.desc.Port, err)
	}
	_ = p.SetReadTimeout(consoleReadTimeout)
	d.port = p
	return p, nil
}

func (d *espDevice) Read(b []byte) (int, error) {
	p, err := d.openConsole()
	if err != nil {
		return 0, err
	}
	return p.Read(b)
}

func (d *espDevice) Write(b []byte) (int, error) {
	p, err := d.openConsole()
	if err != nil {
		return 0, err
	}
	return p.Write(b)
}

func (d *espDevice) Close() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.port == nil {
		return nil
	}
	err := d.port.Close()
	d.port = nil
	return err
}

// EnterBootMode is implemented in Phase 2 via espflasher's DTR/RTS reset.
func (d *espDevice) EnterBootMode(ctx context.Context) error {
	return fmt.Errorf("esp flashing: %w (Phase 2)", ErrUnsupported)
}

// Flash is implemented in Phase 2 via tinygo.org/x/espflasher.
func (d *espDevice) Flash(ctx context.Context, fw io.Reader, size int64, progress func(done, total int64)) error {
	return fmt.Errorf("esp flashing: %w (Phase 2)", ErrUnsupported)
}

// MountDir is never supported for ESP boards (no mass-storage bootloader).
func (d *espDevice) MountDir(ctx context.Context) (string, func() error, error) {
	return "", nil, ErrUnsupported
}

var _ Device = (*espDevice)(nil)
