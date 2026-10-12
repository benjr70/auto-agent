package main

import (
	"os/exec"
	"path/filepath"
	"testing"
)

// The version and commit reach the binary through -ldflags, which no unit
// test can see: these build the real binary the way a release does.
func TestVersionOfABuiltBinary(t *testing.T) {
	const pkg = "github.com/benjr70/auto-agent/internal/version"
	tests := []struct {
		name    string
		ldflags string
		want    string
	}{
		{
			name:    "version prints the injected version and commit",
			ldflags: "-X " + pkg + ".version=v1.2.3 -X " + pkg + ".commit=abc1234",
			want:    "auto-agent v1.2.3 (commit abc1234)\n",
		},
		{
			name: "version prints the fallback when none is injected",
			want: "auto-agent dev (commit unknown)\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bin := filepath.Join(t.TempDir(), "auto-agent")
			build := exec.Command("go", "build", "-buildvcs=false", "-ldflags", tt.ldflags, "-o", bin, ".")
			if out, err := build.CombinedOutput(); err != nil {
				t.Fatalf("go build: %v\n%s", err, out)
			}
			out, err := exec.Command(bin, "version").Output()
			if err != nil {
				t.Fatalf("auto-agent version: %v", err)
			}
			if string(out) != tt.want {
				t.Errorf("auto-agent version printed %q, want %q", out, tt.want)
			}
		})
	}
}
