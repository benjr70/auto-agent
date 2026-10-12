package github

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A stub gh on PATH: the real one is never run by a test.
func stubGH(t *testing.T, script string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte("#!/bin/sh\n"+script+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
}

func TestGHAuthToken(t *testing.T) {
	t.Run("gh is asked for its token and nothing else", func(t *testing.T) {
		stubGH(t, `[ "$*" = "auth token" ] || exit 9; echo gho_fromgh`)
		got, err := GHAuthToken()
		if err != nil || strings.TrimSpace(got) != "gho_fromgh" {
			t.Errorf("GHAuthToken = %q, %v", got, err)
		}
	})
	t.Run("a gh that is logged out, or not installed, is an error", func(t *testing.T) {
		stubGH(t, `echo "not logged in" >&2; exit 1`)
		if _, err := GHAuthToken(); err == nil {
			t.Error("a failing gh gave no error")
		}
		t.Setenv("PATH", t.TempDir())
		if _, err := GHAuthToken(); err == nil {
			t.Error("a missing gh gave no error")
		}
	})
}
