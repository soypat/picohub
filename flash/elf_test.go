package flash

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/soypat/tinyboot/build/uf2"
)

// testdataELF is a real TinyGo build, paired with the .uf2 TinyGo produced from
// the same compile. Converting the ELF ourselves must land on the same image:
// this is the only check that proves the conversion is right rather than merely
// well-formed.
func TestELFToUF2MatchesTinyGo(t *testing.T) {
	tests := []struct {
		name   string
		elf    string
		ref    string
		target Target
		family uint32
	}{
		{"pico", "blink-pico.elf", "blink-pico.uf2", TargetPico, familyRP2040},
		{"pico2", "blink-pico2.elf", "blink-pico2.uf2", TargetPico2, familyRP2350},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			elfPath := filepath.Join("testdata", tt.elf)
			if _, err := os.Stat(elfPath); err != nil {
				t.Skipf("no testdata: %v", err)
			}
			f, err := os.Open(elfPath)
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()

			got, err := ELFToUF2(f, tt.target)
			if err != nil {
				t.Fatal(err)
			}
			want, err := os.ReadFile(filepath.Join("testdata", tt.ref))
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, want) {
				t.Errorf("conversion differs from TinyGo's own uf2: got %d bytes, want %d", len(got), len(want))
				describeFirstDiff(t, got, want)
			}

			// Whatever else is true, the result has to be a UF2 the bootrom
			// will look at: right family, 256-byte payloads, sane addresses.
			blocks, _, err := uf2.DecodeAppendBlocks(nil, bytes.NewReader(got), make([]byte, uf2.BlockSize))
			if err != nil {
				t.Fatalf("our output does not decode as UF2: %v", err)
			}
			if len(blocks) == 0 {
				t.Fatal("no blocks")
			}
			for i, b := range blocks {
				if b.SizeOrFamilyID != tt.family {
					t.Fatalf("block %d family = %#x want %#x", i, b.SizeOrFamilyID, tt.family)
				}
				if b.PayloadSize != uf2ChunkSize {
					t.Fatalf("block %d payload = %d want %d", i, b.PayloadSize, uf2ChunkSize)
				}
			}
			if got, want := blocks[0].TargetAddr, uint32(0x10000000); got != want {
				t.Errorf("image starts at %#x, want flash base %#x", got, want)
			}
		})
	}
}

func describeFirstDiff(t *testing.T, got, want []byte) {
	t.Helper()
	n := min(len(got), len(want))
	for i := 0; i < n; i++ {
		if got[i] != want[i] {
			block, off := i/uf2.BlockSize, i%uf2.BlockSize
			t.Logf("first difference at byte %d (block %d, offset %d): got %#x want %#x", i, block, off, got[i], want[i])
			return
		}
	}
	t.Logf("common prefix of %d bytes is identical; lengths differ", n)
}

// The family id is what stops an image built for one chip being written to the
// other, so getting it wrong is worse than refusing.
func TestFamilyID(t *testing.T) {
	tests := []struct {
		target Target
		want   uint32
		ok     bool
	}{
		{TargetPico, 0xe48bff56, true},
		{TargetPico2, 0xe48bff59, true},
		{TargetESP32C3, 0, false},
		{TargetESP32S3, 0, false},
		{TargetUnknown, 0, false},
	}
	for _, tt := range tests {
		got, ok := tt.target.FamilyID()
		if got != tt.want || ok != tt.ok {
			t.Errorf("%v: FamilyID = %#x,%v want %#x,%v", tt.target, got, ok, tt.want, tt.ok)
		}
	}
}

// An ELF for a chip with no UF2 family id cannot be converted, and must say so
// rather than producing an image nothing will accept.
func TestELFToUF2UnsupportedTarget(t *testing.T) {
	elfPath := filepath.Join("testdata", "blink-pico.elf")
	f, err := os.Open(elfPath)
	if err != nil {
		t.Skipf("no testdata: %v", err)
	}
	defer f.Close()
	if _, err := ELFToUF2(f, TargetESP32C3); err == nil {
		t.Error("converting for a target with no family id should fail")
	}
}

func TestIsELF(t *testing.T) {
	dir := t.TempDir()
	write := func(name string, b []byte) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, b, 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	tests := []struct {
		name string
		path string
		want bool
	}{
		{"elf magic", write("a.bin", []byte{0x7f, 'E', 'L', 'F', 2, 1, 1, 0}), true},
		{"not an elf", write("b.bin", []byte("UF2\n_____")), false},
		{"too short", write("c.bin", []byte{0x7f, 'E'}), false},
		{"empty", write("d.bin", nil), false},
	}
	for _, tt := range tests {
		got, err := IsELF(tt.path)
		if err != nil {
			t.Errorf("%s: %v", tt.name, err)
			continue
		}
		if got != tt.want {
			t.Errorf("%s: IsELF = %v want %v", tt.name, got, tt.want)
		}
	}
	if _, err := IsELF(filepath.Join(dir, "nope")); err == nil {
		t.Error("a missing file should error")
	}
}

// PrepareFirmware is what both the server and the CLI call, so both branches
// and the cleanup contract matter.
func TestPrepareFirmware(t *testing.T) {
	elfPath := filepath.Join("testdata", "blink-pico.elf")
	uf2Path := filepath.Join("testdata", "blink-pico.uf2")
	if _, err := os.Stat(elfPath); err != nil {
		t.Skipf("no testdata: %v", err)
	}

	// A .uf2 is used exactly as it arrived, and nothing is created to clean up.
	got, cleanup, err := PrepareFirmware(uf2Path, TargetPico)
	if err != nil {
		t.Fatal(err)
	}
	if got != uf2Path {
		t.Errorf("a .uf2 should be used as-is, got %q", got)
	}
	cleanup()
	if _, err := os.Stat(uf2Path); err != nil {
		t.Fatalf("cleanup removed the caller's own file: %v", err)
	}

	// An ELF becomes a temporary .uf2 that cleanup removes.
	got, cleanup, err = PrepareFirmware(elfPath, TargetPico)
	if err != nil {
		t.Fatal(err)
	}
	if got == elfPath {
		t.Fatal("an ELF should have been converted")
	}
	converted, err := os.ReadFile(got)
	if err != nil {
		t.Fatal(err)
	}
	reference, err := os.ReadFile(uf2Path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(converted, reference) {
		t.Error("converted file does not match TinyGo's uf2")
	}
	cleanup()
	if _, err := os.Stat(got); !os.IsNotExist(err) {
		t.Errorf("cleanup left %s behind (%v)", got, err)
	}
	if _, err := os.Stat(elfPath); err != nil {
		t.Fatalf("cleanup removed the source ELF: %v", err)
	}

	// Something that is neither is rejected, and says both things it is not.
	junk := filepath.Join(t.TempDir(), "junk.uf2")
	if err := os.WriteFile(junk, []byte("this is not firmware"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := PrepareFirmware(junk, TargetPico); err == nil {
		t.Error("junk should be rejected")
	}
}
