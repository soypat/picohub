package flash

import (
	"fmt"
	"os"

	"github.com/soypat/tinyboot/build/uf2"
)

// ValidateUF2 confirms the file at path decodes as a UF2 image.
func ValidateUF2(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	scratch := make([]byte, uf2.BlockSize)
	blocks, _, err := uf2.DecodeAppendBlocks(nil, f, scratch)
	if err != nil {
		return err
	}
	if len(blocks) == 0 {
		return fmt.Errorf("no UF2 blocks found")
	}
	return nil
}
