package version

import "testing"

func TestString(t *testing.T) {
	tests := []struct {
		name            string
		version, commit string
		want            string
	}{
		{"injected version and commit", "v1.2.3", "abc1234", "auto-agent v1.2.3 (commit abc1234)"},
		{"nothing injected falls back to a development build", "", "", "auto-agent dev (commit unknown)"},
		{"version without a commit", "v1.2.3", "", "auto-agent v1.2.3 (commit unknown)"},
		{"commit without a version", "", "abc1234", "auto-agent dev (commit abc1234)"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := (Info{Version: tt.version, Commit: tt.commit}).String(); got != tt.want {
				t.Errorf("String() = %q, want %q", got, tt.want)
			}
		})
	}
}
