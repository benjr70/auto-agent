package verify

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// Infra is an infrastructure finding: the Host cannot run the round as its
// Surfaces declare. It is reported as such, never read as an item verdict.
type Infra struct{ Reason string }

func (e *Infra) Error() string { return e.Reason }

// The kernel files the Electron sandbox decision reads.
const (
	usernsRestrictFile   = "/proc/sys/kernel/apparmor_restrict_unprivileged_userns"
	appArmorProfilesFile = "/sys/kernel/security/apparmor/profiles"
)

// displayProbeTimeout bounds the default display probe.
const displayProbeTimeout = 10 * time.Second

// Host is what the Surfaces need from the machine a round runs on. The
// binary detects a display and never starts one; it installs nothing.
type Host struct {
	// Env is the Host's environment: the display is its DISPLAY and nothing
	// else, and AUTO_AGENT_ELECTRON_SANDBOX=disabled forces a DEGRADED app.
	Env []string
	// ProbeDisplay answers whether a declared display is up. Nil probes with
	// xdpyinfo when the Host has it, and otherwise takes the display as
	// declared.
	ProbeDisplay func(ctx context.Context, display string) error
	// UsernsRestrictFile and AppArmorProfilesFile are the kernel's switch
	// for unprivileged user namespaces and its loaded-profile list; empty
	// means the kernel's own paths.
	UsernsRestrictFile, AppArmorProfilesFile string
}

// lookup is the last value of key in env.
func lookup(env []string, key string) string {
	value := ""
	for _, kv := range env {
		if k, v, ok := strings.Cut(kv, "="); ok && k == key {
			value = v
		}
	}
	return value
}

// Display is the Host's display, probed before it is trusted. No display,
// or one that does not answer, is an Infra finding: a round never falls back
// to headless.
func (h Host) Display(ctx context.Context) (string, error) {
	display := lookup(h.Env, "DISPLAY")
	if display == "" {
		return "", &Infra{Reason: "no display: DISPLAY is not set in the Host's environment, and a round refuses to fall back to headless"}
	}
	probe := h.ProbeDisplay
	if probe == nil {
		if _, err := exec.LookPath("xdpyinfo"); err != nil {
			return display, nil // nothing to probe with: the display is taken as declared
		}
		probe = xdpyinfo
	}
	if err := probe(ctx, display); err != nil {
		return "", &Infra{Reason: fmt.Sprintf("DISPLAY=%s is declared but does not answer (%v); the Host's display server is down, and a round refuses to fall back to headless", display, err)}
	}
	return display, nil
}

func xdpyinfo(ctx context.Context, display string) error {
	ctx, cancel := context.WithTimeout(ctx, displayProbeTimeout)
	defer cancel()
	return exec.CommandContext(ctx, "xdpyinfo", "-display", display).Run()
}

// Sandbox is how an Electron app's sandbox runs on this Host.
type Sandbox string

const (
	// SandboxOn: the Host does not restrict unprivileged user namespaces.
	SandboxOn Sandbox = "sandbox"
	// SandboxAppArmor: a loaded AppArmor profile grants them to the app.
	SandboxAppArmor Sandbox = "apparmor"
	// SandboxDisabled: the app runs with ELECTRON_DISABLE_SANDBOX=1. The
	// round is DEGRADED and says so; it is never skipped for it.
	SandboxDisabled Sandbox = "disabled"
)

// Degraded is whether the round must report the sandbox as DEGRADED.
func (s Sandbox) Degraded() bool { return s == SandboxDisabled }

// SandboxMode is how the app at bin runs its sandbox on this Host. A missing
// kernel switch reads as unrestricted.
func (h Host) SandboxMode(bin string) Sandbox {
	if lookup(h.Env, "AUTO_AGENT_ELECTRON_SANDBOX") == "disabled" {
		return SandboxDisabled
	}
	restrict, err := os.ReadFile(or(h.UsernsRestrictFile, usernsRestrictFile))
	if err != nil || strings.TrimSpace(string(restrict)) != "1" {
		return SandboxOn
	}
	if profiles, err := os.ReadFile(or(h.AppArmorProfilesFile, appArmorProfilesFile)); err == nil && bin != "" && strings.Contains(string(profiles), bin) {
		return SandboxAppArmor
	}
	return SandboxDisabled
}

func or(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}
