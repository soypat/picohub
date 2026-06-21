package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/soypat/tinyboot/build/uf2"
)

func TestValidateUF2(t *testing.T) {
	dir := t.TempDir()

	// Build a minimal but valid UF2 image with the RP2040 family id.
	data := make([]byte, 1024)
	for i := range data {
		data[i] = byte(i)
	}
	f := &uf2.Formatter{ChunkSize: 256}
	if err := f.SetFamilyID("0xe48bff56"); err != nil { // RP2040
		t.Fatal(err)
	}
	out, n, err := f.AppendTo(nil, data, 0x10000000)
	if err != nil || n == 0 {
		t.Fatalf("format uf2: n=%d err=%v", n, err)
	}
	good := filepath.Join(dir, "good.uf2")
	if err := os.WriteFile(good, out, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := validateUF2(good); err != nil {
		t.Errorf("valid uf2 rejected: %v", err)
	}

	// A non-UF2 file must be rejected.
	bad := filepath.Join(dir, "bad.bin")
	if err := os.WriteFile(bad, []byte("not a uf2 file at all, just text"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := validateUF2(bad); err == nil {
		t.Error("expected non-uf2 file to be rejected")
	}
}
