package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/benjr70/auto-agent/internal/version"
)

func TestRun(t *testing.T) {
	info := version.Info{Version: "v1.2.3", Commit: "abc1234"}
	tests := []struct {
		name       string
		args       []string
		info       version.Info
		wantCode   int
		wantStdout string
		wantStderr []string
	}{
		{
			name:       "version prints the injected version and commit",
			args:       []string{"version"},
			info:       info,
			wantStdout: "auto-agent v1.2.3 (commit abc1234)\n",
		},
		{
			name:       "version prints the fallback when none is injected",
			args:       []string{"version"},
			wantStdout: "auto-agent dev (commit unknown)\n",
		},
		{
			name:       "an unknown command exits non-zero with usage",
			args:       []string{"frobnicate"},
			info:       info,
			wantCode:   2,
			wantStderr: []string{`unknown command "frobnicate"`, "Usage:", "version"},
		},
		{
			name:       "no command exits non-zero with usage",
			info:       info,
			wantCode:   2,
			wantStderr: []string{"Usage:", "version"},
		},
		{
			name:       "version takes no arguments",
			args:       []string{"version", "extra"},
			info:       info,
			wantCode:   2,
			wantStderr: []string{"version takes no arguments", "Usage:"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := Run(tt.args, tt.info, &stdout, &stderr)
			if code != tt.wantCode {
				t.Errorf("exit code = %d, want %d", code, tt.wantCode)
			}
			if stdout.String() != tt.wantStdout {
				t.Errorf("stdout = %q, want %q", stdout.String(), tt.wantStdout)
			}
			for _, want := range tt.wantStderr {
				if !strings.Contains(stderr.String(), want) {
					t.Errorf("stderr = %q, want it to contain %q", stderr.String(), want)
				}
			}
			if len(tt.wantStderr) == 0 && stderr.Len() != 0 {
				t.Errorf("stderr = %q, want it empty", stderr.String())
			}
		})
	}
}
