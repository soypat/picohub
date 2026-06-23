package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Known USB vendor/product ids used to classify boards. VID/PID alone cannot
// always distinguish pico vs pico2 or esp variants; those are refined by a
// per-device Target override (Store) and, for ESP, by espflasher's ChipName at
// flash time. classifyTarget returns the best guess.
const (
	vidRaspberryPi = 0x2e8a
	vidEspressif   = 0x303a
	vidSiLabs      = 0x10c4 // CP210x USB-UART bridges (common on ESP devkits)
	vidWCH         = 0x1a86 // CH340/CH9102 USB-UART bridges
)

// classifyTarget maps a USB vid/pid to a best-guess board Target.
func classifyTarget(vid, pid uint16) Target {
	switch vid {
	case vidRaspberryPi:
		switch pid {
		case 0x000f, 0x0009: // RP2350 BOOTSEL
			return TargetPico2
		default: // 0x0003 RP2040 BOOTSEL, 0x0005 CDC, ... -> assume Pico
			return TargetPico
		}
	case vidEspressif: // native USB-Serial/JTAG (c3, s3, ...)
		return TargetESP32C3
	case vidSiLabs, vidWCH: // UART bridge on an ESP devkit
		return TargetESP32C3
	}
	return TargetUnknown
}

// discoverer enumerates USB serial devices by reading sysfs. The sysfs root is
// configurable so the classification can be unit-tested against a fake tree.
type discoverer struct {
	sysClassTTY string // default "/sys/class/tty"
	devDir      string // default "/dev"
	sysBusUSB   string // default "/sys/bus/usb/devices"; "" disables BOOTSEL scan
}

func newDiscoverer() *discoverer {
	return &discoverer{
		sysClassTTY: "/sys/class/tty",
		devDir:      "/dev",
		sysBusUSB:   "/sys/bus/usb/devices",
	}
}

// scan returns a Descriptor for every USB-backed tty whose vid/pid maps to a
// known board family. Descriptors carry the live Port and a stable ID; Name and
// Target overrides are applied later by the Manager/Store.
func (d *discoverer) scan() ([]Descriptor, error) {
	entries, err := os.ReadDir(d.sysClassTTY)
	if err != nil {
		return nil, err
	}
	var out []Descriptor
	for _, e := range entries {
		name := e.Name() // e.g. ttyACM0, ttyUSB0, tty (skip non-USB)
		if !strings.HasPrefix(name, "ttyACM") && !strings.HasPrefix(name, "ttyUSB") {
			continue
		}
		usbDir := d.usbDeviceDir(name)
		if usbDir == "" {
			continue // not a USB-backed tty
		}
		vid, ok1 := readHexAttr(usbDir, "idVendor")
		pid, ok2 := readHexAttr(usbDir, "idProduct")
		if !ok1 || !ok2 {
			continue
		}
		target := classifyTarget(vid, pid)
		if target == TargetUnknown {
			continue
		}
		out = append(out, Descriptor{
			ID:      stableID(vid, pid, readAttr(usbDir, "serial"), filepath.Base(usbDir)),
			Target:  target,
			Port:    filepath.Join(d.devDir, name),
			VID:     vid,
			PID:     pid,
			Present: true,
		})
	}
	return append(out, d.scanBootsel()...), nil
}

// rp2BootselPID reports whether vid/pid is an RP2 bootrom in BOOTSEL mode.
// These boards expose USB mass storage, not a tty, so the tty scan misses them.
func rp2BootselPID(vid, pid uint16) bool {
	return vid == vidRaspberryPi && (pid == 0x0003 || pid == 0x000f || pid == 0x0009)
}

// scanBootsel walks the USB device tree for boards currently in BOOTSEL mode
// and returns a Descriptor (with no Port) for each. Returns nil if the scan is
// disabled (sysBusUSB == "") or the tree is unreadable.
func (d *discoverer) scanBootsel() []Descriptor {
	if d.sysBusUSB == "" {
		return nil
	}
	entries, err := os.ReadDir(d.sysBusUSB)
	if err != nil {
		return nil
	}
	var out []Descriptor
	for _, e := range entries {
		name := e.Name()
		if strings.ContainsRune(name, ':') {
			continue // an interface (e.g. "1-10:1.0"), not a device
		}
		dir := filepath.Join(d.sysBusUSB, name)
		vid, ok1 := readHexAttr(dir, "idVendor")
		pid, ok2 := readHexAttr(dir, "idProduct")
		if !ok1 || !ok2 || !rp2BootselPID(vid, pid) {
			continue
		}
		out = append(out, Descriptor{
			ID:      stableID(vid, pid, readAttr(dir, "serial"), name),
			Target:  classifyTarget(vid, pid),
			VID:     vid,
			PID:     pid,
			Present: true,
			BootSel: true,
		})
	}
	return out
}

// usbDeviceDir resolves /sys/class/tty/<name>/device and walks up to the USB
// device directory (the one containing idVendor). Returns "" if not USB-backed.
func (d *discoverer) usbDeviceDir(ttyName string) string {
	dir, err := filepath.EvalSymlinks(filepath.Join(d.sysClassTTY, ttyName, "device"))
	if err != nil {
		return ""
	}
	for i := 0; i < 8 && dir != "/" && dir != "."; i++ {
		if _, err := os.Stat(filepath.Join(dir, "idVendor")); err == nil {
			return dir
		}
		dir = filepath.Dir(dir)
	}
	return ""
}

// stableID prefers the USB serial number; if absent (common on CP210x/CH340),
// it falls back to a vid:pid + topology path id that is stable per physical
// USB port.
func stableID(vid, pid uint16, serial, usbPath string) string {
	if s := strings.TrimSpace(serial); s != "" {
		return s
	}
	return strings.ToLower(hex4(vid)+":"+hex4(pid)) + "@" + usbPath
}

func readAttr(dir, attr string) string {
	b, err := os.ReadFile(filepath.Join(dir, attr))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// readHexAttr reads a 4-hex-digit sysfs attribute (e.g. "2e8a") as a uint16.
func readHexAttr(dir, attr string) (uint16, bool) {
	s := readAttr(dir, attr)
	if s == "" {
		return 0, false
	}
	v, err := strconv.ParseUint(s, 16, 16)
	if err != nil {
		return 0, false
	}
	return uint16(v), true
}

func hex4(v uint16) string {
	s := strconv.FormatUint(uint64(v), 16)
	return strings.Repeat("0", 4-len(s)) + s
}
