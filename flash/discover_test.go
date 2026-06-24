package flash

import (
	"os"
	"path/filepath"
	"testing"
)

func TestClassifyTarget(t *testing.T) {
	cases := []struct {
		vid, pid uint16
		want     Target
	}{
		{0x2e8a, 0x0003, TargetPico},    // RP2040 BOOTSEL
		{0x2e8a, 0x0005, TargetPico},    // Pico CDC
		{0x2e8a, 0x000f, TargetPico2},   // RP2350 BOOTSEL
		{0x2e8a, 0x0009, TargetPico2},   // RP2350 BOOTSEL (alt)
		{0x303a, 0x1001, TargetESP32C3}, // Espressif native USB
		{0x10c4, 0xea60, TargetESP32C3}, // CP2102
		{0x1a86, 0x7523, TargetESP32C3}, // CH340
		{0x1234, 0x5678, TargetUnknown},
	}
	for _, c := range cases {
		if got := classifyTarget(c.vid, c.pid); got != c.want {
			t.Errorf("classifyTarget(%#04x,%#04x)=%v want %v", c.vid, c.pid, got, c.want)
		}
	}
}

// writeUSBDev creates a fake sysfs USB device dir and a tty whose "device"
// symlink points at it.
func writeUSBDev(t *testing.T, root, tty, vid, pid, serial string) {
	t.Helper()
	usbDir := filepath.Join(root, "usb", tty)
	if err := os.MkdirAll(usbDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, val := range map[string]string{"idVendor": vid, "idProduct": pid, "serial": serial} {
		if val == "" {
			continue
		}
		if err := os.WriteFile(filepath.Join(usbDir, name), []byte(val+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	ttyDir := filepath.Join(root, "sys", "class", "tty", tty)
	if err := os.MkdirAll(ttyDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(usbDir, filepath.Join(ttyDir, "device")); err != nil {
		t.Fatal(err)
	}
}

func TestDiscovererScan(t *testing.T) {
	root := t.TempDir()
	writeUSBDev(t, root, "ttyACM0", "2e8a", "0005", "E460000111") // Pico
	writeUSBDev(t, root, "ttyUSB0", "10c4", "ea60", "")           // CP2102, no serial
	writeUSBDev(t, root, "ttyACM9", "1234", "5678", "x")          // unknown vid -> skipped
	// A tty with no "device" symlink (e.g. virtual console) -> skipped.
	if err := os.MkdirAll(filepath.Join(root, "sys", "class", "tty", "tty0"), 0o755); err != nil {
		t.Fatal(err)
	}

	d := &Discoverer{sysClassTTY: filepath.Join(root, "sys", "class", "tty"), devDir: "/dev"}
	got, err := d.Scan()
	if err != nil {
		t.Fatal(err)
	}
	byPort := map[string]Descriptor{}
	for _, g := range got {
		byPort[g.Port] = g
	}
	if len(byPort) != 2 {
		t.Fatalf("expected 2 known devices, got %d: %+v", len(byPort), got)
	}

	pico, ok := byPort["/dev/ttyACM0"]
	if !ok {
		t.Fatal("ttyACM0 not discovered")
	}
	if pico.Target != TargetPico || pico.VID != 0x2e8a || pico.PID != 0x0005 {
		t.Errorf("pico descriptor wrong: %+v", pico)
	}
	if pico.ID != "E460000111" {
		t.Errorf("pico ID should be serial number, got %q", pico.ID)
	}

	cp, ok := byPort["/dev/ttyUSB0"]
	if !ok {
		t.Fatal("ttyUSB0 not discovered")
	}
	// No serial number: ID falls back to vid:pid@path.
	if cp.ID == "" || cp.ID == "E460000111" {
		t.Errorf("cp2102 fallback ID wrong: %q", cp.ID)
	}
}

// writeUSBBusDev creates a fake /sys/bus/usb/devices entry for a raw USB device
// (no tty), as a BOOTSEL board appears.
func writeUSBBusDev(t *testing.T, bus, name, vid, pid, serial string) {
	t.Helper()
	dir := filepath.Join(bus, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for n, v := range map[string]string{"idVendor": vid, "idProduct": pid, "serial": serial} {
		if v == "" {
			continue
		}
		if err := os.WriteFile(filepath.Join(dir, n), []byte(v+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestScanBootsel(t *testing.T) {
	bus := t.TempDir()
	writeUSBBusDev(t, bus, "1-10", "2e8a", "0003", "E0C9125B0D9B") // RP2040 BOOTSEL
	writeUSBBusDev(t, bus, "1-10:1.0", "2e8a", "0003", "")         // its interface -> skipped
	writeUSBBusDev(t, bus, "2-1", "2e8a", "0005", "RUN123")        // CDC (running) -> not bootsel
	writeUSBBusDev(t, bus, "usb1", "1d6b", "0002", "")             // root hub -> skipped

	d := &Discoverer{sysClassTTY: t.TempDir(), devDir: "/dev", sysBusUSB: bus}
	got, err := d.Scan()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 bootsel device, got %d: %+v", len(got), got)
	}
	b := got[0]
	if !b.BootSel || b.ID != "E0C9125B0D9B" || b.Target != TargetPico || b.Port != "" {
		t.Errorf("bootsel descriptor wrong: %+v", b)
	}
}

// A discoverer with sysBusUSB unset must not scan the host's real USB tree.
func TestScanBootselDisabled(t *testing.T) {
	d := &Discoverer{sysClassTTY: t.TempDir(), devDir: "/dev"}
	if got := d.scanBootsel(); got != nil {
		t.Errorf("expected nil with sysBusUSB unset, got %+v", got)
	}
}
