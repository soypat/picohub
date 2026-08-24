package flash

import (
	"bytes"
	"debug/elf"
	"errors"
	"fmt"
	"io"
	"math"
	"os"

	"github.com/soypat/tinyboot/build/elfutil"
	"github.com/soypat/tinyboot/build/uf2"
)

// UF2 family ids, as the RP2 bootrom expects them. They match the
// uf2-family-id of TinyGo's rp2040 and rp2350 targets: the bootrom checks the
// id and silently refuses an image built for the other chip.
const (
	familyRP2040 = 0xe48bff56
	familyRP2350 = 0xe48bff59 // RP2350 Arm Secure image
)

// rp2FlashEnd is the first address that is not flash on an RP2 chip — SRAM
// starts here — so anything at or above it belongs in RAM and is not part of
// the image written to flash.
const rp2FlashEnd = 0x20000000

// maxROMSize bounds the image built from an ELF. The largest RP2 board in
// circulation has 16 MiB of flash; anything past that is a malformed or
// mislabelled ELF, and building it would waste memory to produce a UF2 that
// cannot be written anyway.
const maxROMSize = 16 << 20

// uf2ChunkSize is the payload size per UF2 block. The RP2 bootrom requires
// 256, matching a flash page.
const uf2ChunkSize = 256

// elfMagic is "\x7fELF".
var elfMagic = []byte{0x7f, 'E', 'L', 'F'}

// FamilyID is the UF2 family id the target's bootrom expects, and whether the
// target has one at all.
func (t Target) FamilyID() (uint32, bool) {
	switch t {
	case TargetPico:
		return familyRP2040, true
	case TargetPico2:
		return familyRP2350, true
	}
	return 0, false
}

// IsELF reports whether the file at path begins with the ELF magic. Firmware
// arrives from a browser upload, so the contents decide what it is; the
// filename is only a hint.
func IsELF(path string) (bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer f.Close()
	var head [4]byte
	n, err := io.ReadFull(f, head[:])
	if err != nil && n < len(head) {
		return false, nil // too short to be an ELF; not an error, just not one
	}
	return bytes.Equal(head[:], elfMagic), nil
}

// ELFToUF2 converts an ELF's read-only memory image into a UF2 for the given
// target. This is the same conversion `picobin uf2conv` and TinyGo's own
// build step do: take the contiguous flash image the ELF describes and wrap it
// in UF2 blocks tagged with the chip's family id.
//
// Debug information is dropped — a UF2 carries none — so keep the ELF if you
// intend to debug what you flashed.
func ELFToUF2(r io.ReaderAt, t Target) ([]byte, error) {
	familyID, ok := t.FamilyID()
	if !ok {
		return nil, fmt.Errorf("%w: no UF2 family id for target %s", ErrUnsupported, t)
	}
	f, err := elf.NewFile(r)
	if err != nil {
		return nil, fmt.Errorf("reading ELF: %w", err)
	}
	rom, romAddr, err := elfROM(f)
	if err != nil {
		return nil, err
	}
	if romAddr+uint64(len(rom)) > math.MaxUint32 {
		return nil, fmt.Errorf("image ends at %#x, which overflows the 32-bit UF2 address", romAddr+uint64(len(rom)))
	}
	// Pad to a whole number of blocks. The RP2 bootrom only accepts UF2 blocks
	// whose payload is exactly 256 bytes and skips any that are not, so a short
	// final block would be dropped and the tail of the firmware would never be
	// written — a flash that reports success and leaves a broken image.
	if rem := len(rom) % uf2ChunkSize; rem != 0 {
		rom = append(rom, make([]byte, uf2ChunkSize-rem)...)
	}
	formatter := uf2.Formatter{
		ChunkSize: uf2ChunkSize,
		FamilyID:  familyID,
		Flags:     uf2.FlagFamilyIDPresent,
	}
	out := make([]byte, 0, len(rom)+len(rom)/uf2.BlockSize*(32+4))
	out, blocks, err := formatter.AppendTo(out, rom, uint32(romAddr))
	if err != nil {
		return nil, fmt.Errorf("building UF2: %w", err)
	}
	if blocks == 0 {
		return nil, errors.New("ELF produced no UF2 blocks")
	}
	return out, nil
}

// elfROM extracts the contiguous flash image an ELF describes, and the address
// it starts at. Sections and program headers that live above the start of SRAM
// are discarded first: they are RAM contents, not part of the flash image.
func elfROM(f *elf.File) (rom []byte, romAddr uint64, err error) {
	var sections []*elf.Section
	for _, sect := range f.Sections {
		if elfutil.SectionIsROM(sect) && sect.Addr < rp2FlashEnd {
			sections = append(sections, sect)
		}
	}
	var progs []*elf.Prog
	for _, prog := range f.Progs {
		if elfutil.ProgIsROM(prog) && prog.Paddr < rp2FlashEnd {
			progs = append(progs, prog)
		}
	}
	f.Sections, f.Progs = sections, progs

	start, end, err := elfutil.ROMAddr(f)
	if err != nil {
		return nil, 0, fmt.Errorf("locating ELF flash image: %w", err)
	}
	if err := elfutil.EnsureROMContiguous(f, start, end, 0); err != nil {
		return nil, start, fmt.Errorf("ELF flash image is not contiguous: %w", err)
	}
	size := end - start
	if size > maxROMSize {
		// Deliberately an error rather than a truncation: writing a partial
		// image would leave the board running something that is not the
		// firmware anyone built.
		return nil, start, fmt.Errorf("ELF flash image is %d bytes, more than the %d byte limit", size, maxROMSize)
	}
	rom = make([]byte, size)
	n, err := elfutil.ReadROMAt(f, rom, start)
	if err != nil {
		return nil, start, fmt.Errorf("reading ELF flash image: %w", err)
	}
	return rom[:n], start, nil
}

// PrepareFirmware returns the path of a .uf2 ready to write to a board, and a
// cleanup to call when it has been written. An upload that is already a .uf2 is
// validated and used as-is; an ELF is converted for the board's target.
//
// cleanup is never nil, and removes only files this function created — the
// caller's own file is left alone.
func PrepareFirmware(path string, t Target) (uf2Path string, cleanup func(), err error) {
	noop := func() {}
	isELF, err := IsELF(path)
	if err != nil {
		return "", noop, err
	}
	if !isELF {
		if err := ValidateUF2(path); err != nil {
			return "", noop, fmt.Errorf("not a valid .uf2 (and not an ELF): %w", err)
		}
		return path, noop, nil
	}

	src, err := os.Open(path)
	if err != nil {
		return "", noop, err
	}
	defer src.Close()
	image, err := ELFToUF2(src, t)
	if err != nil {
		return "", noop, err
	}

	tmp, err := os.CreateTemp("", "picohub-elf-*.uf2")
	if err != nil {
		return "", noop, err
	}
	name := tmp.Name()
	remove := func() { os.Remove(name) }
	if _, err := tmp.Write(image); err != nil {
		tmp.Close()
		remove()
		return "", noop, err
	}
	if err := tmp.Close(); err != nil {
		remove()
		return "", noop, err
	}
	return name, remove, nil
}
