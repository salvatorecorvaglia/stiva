package storage

import (
	"errors"
	"os"
)

func (fs *FilesystemEngine) probeDisk() error {
	f, err := os.CreateTemp(fs.dataDir, ".stiva-probe-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	_, writeErr := f.Write([]byte("stiva readiness\n"))
	syncErr := f.Sync()
	closeErr := f.Close()
	return errors.Join(writeErr, syncErr, closeErr)
}
