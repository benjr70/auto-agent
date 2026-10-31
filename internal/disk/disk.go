// Package disk reads what the Disk guard and the doctor ask of the Host's
// disk: how much space is free, said in the units the Harness config
// declares the need in.
package disk

import (
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"syscall"
)

// Free is the space an unprivileged process can still write on the
// filesystem holding path, in bytes. A path not made yet, such as a State
// dir before the first Fire, is on the filesystem of the nearest directory
// above it that exists.
func Free(path string) (uint64, error) {
	for at := path; ; at = filepath.Dir(at) {
		var stat syscall.Statfs_t
		err := syscall.Statfs(at, &stat)
		if err == nil {
			return stat.Bavail * uint64(stat.Bsize), nil
		}
		if !errors.Is(err, fs.ErrNotExist) || at == filepath.Dir(at) {
			return 0, fmt.Errorf("reading the free space of %s: %w", at, err)
		}
	}
}

// Size is bytes as a person reads them: whole MB under a GB, else GB or TB
// to one decimal, each unit 1024 of the one before, as the Harness config's
// disk_free counts them.
func Size(bytes uint64) string {
	switch {
	case bytes >= 1<<40:
		return fmt.Sprintf("%.1f TB", float64(bytes)/(1<<40))
	case bytes >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(bytes)/(1<<30))
	}
	return fmt.Sprintf("%d MB", bytes>>20)
}
