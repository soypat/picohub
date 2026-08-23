// Package flash provides board discovery and firmware-flashing for
// locally-connected USB boards (Raspberry Pi Pico / Pico2 via the BOOTSEL
// mass-storage flow, and ESP32c3/s3 via espflasher). It is independent of the
// picohub HTTP server so it can be driven directly from a CLI or tests.
package flash

// Target identifies a board family and, implicitly, its flashing method.
type Target int

const (
	TargetUnknown Target = iota
	TargetPico           // RP2040: BOOTSEL mass-storage, .uf2
	TargetPico2          // RP2350: BOOTSEL mass-storage, .uf2
	TargetESP32C3        // espflasher over serial, .bin
	TargetESP32S3        // espflasher over serial, .bin
)

// Targets lists every board family a user may pick from, in display order.
// TargetUnknown is not included: it means "whatever discovery said".
func Targets() []Target {
	return []Target{TargetPico, TargetPico2, TargetESP32C3, TargetESP32S3}
}

// ParseTarget resolves a family name as produced by Target.String. It reports
// whether the name was known; "" and "unknown" both give TargetUnknown, which
// is how a caller says "use the discovered family".
func ParseTarget(s string) (Target, bool) {
	switch s {
	case "", "unknown":
		return TargetUnknown, true
	case "pico":
		return TargetPico, true
	case "pico2":
		return TargetPico2, true
	case "esp32c3":
		return TargetESP32C3, true
	case "esp32s3":
		return TargetESP32S3, true
	}
	return TargetUnknown, false
}

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
