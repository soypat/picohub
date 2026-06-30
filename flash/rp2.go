package flash

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"go.bug.st/serial"
)

// rp2BootselLabels are the FS labels the RP2 bootrom exposes in BOOTSEL mode.
// RP2040 -> RPI-RP2, RP2350 -> RP2350; RPI2 is seen on some clones.
var rp2BootselLabels = []string{"RPI-RP2", "RP2350", "RPI2"}

// rp2Device flashes Raspberry Pi Pico / Pico2 boards via the BOOTSEL
// mass-storage flow, mirroring cmd/_import-examples/picodeploy/deploy.sh.
type rp2Device struct {
	desc Descriptor

	mu   sync.Mutex
	port serial.Port // console connection, lazily opened by Read/Write
}

func newRP2Device(desc Descriptor) *rp2Device { return &rp2Device{desc: desc} }

func (d *rp2Device) Descriptor() Descriptor { return d.desc }

// openConsole opens (once) the serial console port for monitoring.
func (d *rp2Device) openConsole() (serial.Port, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.port != nil {
		return d.port, nil
	}
	p, err := serial.Open(d.desc.Port, &serial.Mode{BaudRate: consoleBaud})
	if err != nil {
		return nil, fmt.Errorf("open console %s: %w", d.desc.Port, err)
	}
	// A read timeout lets the console pump poll for shutdown between reads.
	_ = p.SetReadTimeout(consoleReadTimeout)
	d.port = p
	return p, nil
}

func (d *rp2Device) Read(b []byte) (int, error) {
	p, err := d.openConsole()
	if err != nil {
		return 0, err
	}
	return p.Read(b)
}

func (d *rp2Device) Write(b []byte) (int, error) {
	p, err := d.openConsole()
	if err != nil {
		return 0, err
	}
	return p.Write(b)
}

func (d *rp2Device) Close() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.port == nil {
		return nil
	}
	err := d.port.Close()
	d.port = nil
	return err
}

// EnterBootMode performs the "1200-baud touch": open the CDC port at 1200 baud
// and DROP DTR, which trips machine.EnterBootloader() in TinyGo's USB stack.
// The board resets and drops off the bus mid-touch, so errors are expected and
// swallowed. The console connection is closed first to free the port.
func (d *rp2Device) EnterBootMode(ctx context.Context) error {
	_ = d.Close()

	// If the bootsel volume is already present, the board is already in BOOTSEL
	// (e.g. user held BOOTSEL on power-up): nothing to reset.
	if dev := findBootselBlockDev(); dev != "" {
		return nil
	}

	p, err := serial.Open(d.desc.Port, &serial.Mode{BaudRate: 1200})
	if err != nil {
		// Port may already be gone; that is not necessarily a failure.
		return nil
	}
	_ = p.SetDTR(false)
	select {
	case <-time.After(200 * time.Millisecond):
	case <-ctx.Done():
	}
	_ = p.Close()
	return nil
}

// MountDir resets the board into BOOTSEL (if needed), waits for the
// mass-storage volume to appear, mounts it, and returns the mountpoint plus an
// unmount function.
func (d *rp2Device) MountDir(ctx context.Context) (string, func() error, error) {
	if err := d.EnterBootMode(ctx); err != nil {
		return "", nil, err
	}

	dev, err := waitBootselBlockDev(ctx, 30*time.Second)
	if err != nil {
		return "", nil, err
	}

	// Already mounted (desktop automount)? Reuse it, don't unmount on cleanup.
	if mnt := findmnt(dev); mnt != "" {
		return mnt, func() error { return nil }, nil
	}

	// Each mount strategy that fails appends why it failed, so a hard failure
	// reports the real cause (polkit denial, missing sudoers rule, ...) instead
	// of a bare exit status.
	var why []string

	// Mount via udisksctl (root-free, mounts under /run/media/$USER).
	if _, err := exec.LookPath("udisksctl"); err == nil {
		out, err := exec.CommandContext(ctx, "udisksctl", "mount", "-b", dev).CombinedOutput()
		if err == nil {
			mnt := findmnt(dev)
			if mnt == "" {
				mnt = parseUdisksMount(string(out))
			}
			if mnt != "" {
				unmount := func() error {
					return exec.Command("udisksctl", "unmount", "-b", dev).Run()
				}
				return mnt, unmount, nil
			}
			why = append(why, "udisksctl reported success but no mountpoint found: "+oneLine(out))
		} else {
			why = append(why, fmt.Sprintf("udisksctl: %v: %s", err, oneLine(out)))
		}
	} else {
		why = append(why, "udisksctl not found in PATH")
	}

	// Fallback: non-interactive sudo mount to a temp dir, owned by us.
	mnt, err := os.MkdirTemp("", "picohub-rp2-")
	if err != nil {
		return "", nil, err
	}
	uidgid := fmt.Sprintf("uid=%d,gid=%d", os.Getuid(), os.Getgid())
	if out, err := exec.CommandContext(ctx, "sudo", "-n", "mount", "-o", uidgid, dev, mnt).CombinedOutput(); err != nil {
		_ = os.Remove(mnt)
		why = append(why, fmt.Sprintf("sudo -n mount: %v: %s", err, oneLine(out)))
		return "", nil, fmt.Errorf("could not mount %s:\n\t- %s", dev, strings.Join(why, "\n\t- "))
	}
	unmount := func() error {
		err := exec.Command("sudo", "-n", "umount", mnt).Run()
		_ = os.Remove(mnt)
		return err
	}
	return mnt, unmount, nil
}

// Flash enters BOOTSEL, mounts the volume, copies the .uf2, and unmounts. The
// bootrom reboots the instant the .uf2 lands, so a copy error after the device
// vanished is the SUCCESS path (matching deploy.sh).
func (d *rp2Device) Flash(ctx context.Context, fw io.Reader, size int64, progress func(done, total int64)) error {
	mnt, unmount, err := d.MountDir(ctx)
	if err != nil {
		return err
	}
	defer unmount()

	dst := filepath.Join(mnt, "firmware.uf2")
	f, err := os.Create(dst)
	if err != nil {
		return fmt.Errorf("create %s: %w", dst, err)
	}

	written, copyErr := copyWithProgress(f, fw, size, progress)
	syncErr := f.Sync()
	closeErr := f.Close()

	// If the volume vanished, the bootrom accepted the image and rebooted.
	if findBootselBlockDev() == "" {
		return nil
	}
	if copyErr != nil {
		return fmt.Errorf("copy firmware (%d bytes written): %w", written, copyErr)
	}
	if syncErr != nil {
		return fmt.Errorf("sync firmware: %w", syncErr)
	}
	return closeErr
}

// --- block-device discovery / mount helpers --------------------------------

// findBootselBlockDev returns the /dev path of the first block device whose FS
// label is a known BOOTSEL label, or "" if none is present.
func findBootselBlockDev() string {
	for _, label := range rp2BootselLabels {
		link := filepath.Join("/dev/disk/by-label", label)
		if dev, err := filepath.EvalSymlinks(link); err == nil {
			return dev
		}
	}
	return ""
}

// waitBootselBlockDev polls for a BOOTSEL block device until it appears or the
// timeout/context elapses.
func waitBootselBlockDev(ctx context.Context, timeout time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)
	for {
		if dev := findBootselBlockDev(); dev != "" {
			return dev, nil
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("timed out waiting for a BOOTSEL volume (%s)", strings.Join(rp2BootselLabels, ", "))
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(300 * time.Millisecond):
		}
	}
}

// findmnt returns the mountpoint of dev, or "" if it is not mounted.
func findmnt(dev string) string {
	out, err := exec.Command("findmnt", "-nro", "TARGET", dev).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(strings.SplitN(string(out), "\n", 2)[0])
}

// oneLine collapses command output to a single trimmed line for error messages.
func oneLine(b []byte) string {
	s := strings.TrimSpace(string(b))
	if s == "" {
		return "(no output)"
	}
	return strings.Join(strings.Fields(s), " ")
}

// parseUdisksMount extracts the mountpoint from "Mounted /dev/sdb1 at /path."
func parseUdisksMount(out string) string {
	const marker = " at "
	i := strings.LastIndex(out, marker)
	if i < 0 {
		return ""
	}
	return strings.TrimRight(strings.TrimSpace(out[i+len(marker):]), ".")
}

// copyWithProgress copies src to dst, invoking progress (if non-nil) as bytes
// flow. total is the expected size for the progress callback (0 if unknown).
func copyWithProgress(dst io.Writer, src io.Reader, total int64, progress func(done, total int64)) (int64, error) {
	if progress == nil {
		return io.Copy(dst, src)
	}
	buf := make([]byte, 64*1024)
	var done int64
	for {
		n, err := src.Read(buf)
		if n > 0 {
			if _, werr := dst.Write(buf[:n]); werr != nil {
				return done, werr
			}
			done += int64(n)
			progress(done, total)
		}
		if err == io.EOF {
			return done, nil
		}
		if err != nil {
			return done, err
		}
	}
}

var _ Device = (*rp2Device)(nil)
