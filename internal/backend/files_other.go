// files_other.go: the link-safe write exists on Linux, where ccsbx-root runs; elsewhere (dev
// builds) a generation file is written plainly, refusing only an existing file.

//go:build !linux

package backend

import (
	"os"
	"path/filepath"
)

func writeGenFile(genDir, name string, data []byte) error {
	f, err := os.OpenFile(filepath.Join(genDir, name), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(data)
	return err
}
