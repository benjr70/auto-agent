package disk

import (
	"path/filepath"
	"testing"
)

func TestFreeReadsTheNearestDirectoryThatExists(t *testing.T) {
	dir := t.TempDir()
	here, err := Free(dir)
	if err != nil || here == 0 {
		t.Fatalf("Free(%s) = %d, %v", dir, here, err)
	}
	// A State dir not made yet is on its parent's filesystem.
	if _, err := Free(filepath.Join(dir, "state", "not", "yet")); err != nil {
		t.Errorf("Free of a directory not made yet: %v", err)
	}
}

func TestSize(t *testing.T) {
	for bytes, want := range map[uint64]string{
		10 << 30:        "10.0 GB",
		3 << 29:         "1.5 GB",
		512 << 20:       "512 MB",
		0:               "0 MB",
		2<<40 + 1<<39:   "2.5 TB",
		1023<<20 + 1000: "1023 MB",
	} {
		if got := Size(bytes); got != want {
			t.Errorf("Size(%d) = %q, want %q", bytes, got, want)
		}
	}
}
