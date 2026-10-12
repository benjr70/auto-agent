package version

import (
	"runtime/debug"
	"testing"
)

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

func TestResolve(t *testing.T) {
	const revision = "be7b3c80dd63dab9228977f7491a631b65066e0c"
	stamped := func(version string, settings ...debug.BuildSetting) *debug.BuildInfo {
		return &debug.BuildInfo{Main: debug.Module{Version: version}, Settings: settings}
	}
	vcs := debug.BuildSetting{Key: "vcs.revision", Value: revision}
	tests := []struct {
		name     string
		injected Info
		stamped  *debug.BuildInfo
		want     Info
	}{
		{
			name:     "injected values win over stamped ones",
			injected: Info{Version: "v1.2.3", Commit: "abc1234"},
			stamped:  stamped("v9.9.9", vcs),
			want:     Info{Version: "v1.2.3", Commit: "abc1234"},
		},
		{
			name:    "nothing injected takes the stamped version and a shortened revision",
			stamped: stamped("v0.4.0", vcs),
			want:    Info{Version: "v0.4.0", Commit: "be7b3c80dd63"},
		},
		{
			name:     "an injected version keeps the stamped revision",
			injected: Info{Version: "v1.2.3"},
			stamped:  stamped("v0.4.0", vcs),
			want:     Info{Version: "v1.2.3", Commit: "be7b3c80dd63"},
		},
		{
			name:    "a build with no version control is a development build",
			stamped: stamped("(devel)"),
			want:    Info{},
		},
		{
			name: "a binary with no build information is a development build",
			want: Info{},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := resolve(tt.injected, tt.stamped); got != tt.want {
				t.Errorf("resolve() = %+v, want %+v", got, tt.want)
			}
		})
	}
}
