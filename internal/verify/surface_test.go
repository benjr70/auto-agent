package verify

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/benjr70/auto-agent/internal/checklist"
	"github.com/benjr70/auto-agent/internal/config"
)

// fakeApp is a Target Project's verify/app: it records how it was launched,
// its pid, and then idles until it is stopped.
const fakeApp = `#!/bin/sh
{ echo "args: $*"; echo "PANEL_URL=${PANEL_URL:-}"; echo "DISPLAY=${DISPLAY:-}"; echo "SANDBOX_FLAG=${ELECTRON_DISABLE_SANDBOX:-unset}"; } > "$APP_RECORD.tmp"
mv "$APP_RECORD.tmp" "$APP_RECORD"
echo $$ >> "$APP_RECORD.pids"
exec sleep 30
`

var launchSurfaces = map[string]config.Surface{
	"web":   {Kind: "browser", URLKey: "WEB_URL", Viewport: "427x952"},
	"panel": {Kind: "electron", URLKey: "PANEL_URL", Launcher: "verify/app"},
	"bare":  {Kind: "electron", URLKey: "BARE_URL"},
	"api":   {Kind: "api", URLKey: "API_URL"},
}

var launchBlock = [][2]string{{"WEB_URL", "http://web.test"}, {"PANEL_URL", "http://panel.test"}, {"BARE_URL", "http://bare.test"}}

type launchFixture struct {
	l      Launcher
	record string
}

// newLaunch is a Launcher over a stub Target Project whose app answers on
// its DevTools endpoint once it has really run. The display is declared and
// its probe stubbed, so no display server is needed.
func newLaunch(t *testing.T, env ...string) *launchFixture {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "verify"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "verify", "app"), []byte(fakeApp), 0o755); err != nil {
		t.Fatal(err)
	}
	record := filepath.Join(t.TempDir(), "app")
	f := &launchFixture{record: record}
	f.l = Launcher{
		Host: Host{
			Env:                append([]string{"PATH=" + os.Getenv("PATH"), "DISPLAY=:99", "APP_RECORD=" + record}, env...),
			ProbeDisplay:       probeOK,
			UsernsRestrictFile: filepath.Join(t.TempDir(), "missing"),
		},
		Dir:        dir,
		RunDir:     filepath.Join(t.TempDir(), "run"),
		MCPCommand: []string{"mcp-stub", "--stub"},
		ProbeCDP: func(context.Context, string) error {
			_, err := os.Stat(record)
			return err
		},
		Wait:     5 * time.Second,
		Interval: 10 * time.Millisecond,
	}
	return f
}

func (f *launchFixture) launch(t *testing.T, apps ...string) (*Session, error) {
	t.Helper()
	s, err := f.l.Launch(context.Background(), launchSurfaces, apps, launchBlock)
	t.Cleanup(func() { s.Close() })
	return s, err
}

func (f *launchFixture) pids(t *testing.T) []int {
	t.Helper()
	text, _ := os.ReadFile(f.record + ".pids")
	var pids []int
	for _, line := range strings.Fields(string(text)) {
		pid, _ := strconv.Atoi(line)
		pids = append(pids, pid)
	}
	return pids
}

func alive(pid int) bool { return syscall.Kill(pid, 0) == nil }

func arg(args []string, flag string) string {
	for i, a := range args {
		if a == flag && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

func isInfra(err error, want string) bool {
	var infra *Infra
	return errors.As(err, &infra) && strings.Contains(infra.Reason, want)
}

// Ported from lib/surface-launch.test.sh and the app-launch half of
// lib/verify-boot.test.sh, under their case names. Go hands the Step its MCP
// configuration on the call, so the cases about a CLI that re-launches
// itself (`mcp-config --out`, the `usage:` cases) are dropped, and the
// mcp-config entries run the MCP server directly. A case whose bash name
// promised an exit code is asserted on the error Go returns instead.
func TestLaunch(t *testing.T) {
	t.Run("mcp browser: the kind chose the launcher", func(t *testing.T) {
		f := newLaunch(t)
		s, err := f.launch(t)
		web := s.MCP["surface-web"]
		if err != nil || web.Command != "mcp-stub" || web.Args[0] != "--stub" || arg(web.Args, "--browser") != "chrome" ||
			arg(web.Args, "--viewport-size") != "427,952" || !strings.HasPrefix(arg(web.Args, "--user-data-dir"), filepath.Join(f.l.RunDir, "profiles", "web-")) ||
			web.Env["DISPLAY"] != ":99" {
			t.Errorf("surface-web = %+v, %v", web, err)
		}
	})
	t.Run("mcp browser: a declared viewport, else the default", func(t *testing.T) {
		f := newLaunch(t)
		s, err := f.l.Launch(context.Background(), map[string]config.Surface{"web": {Kind: "browser", URLKey: "WEB_URL"}}, nil, launchBlock)
		defer s.Close()
		if got := arg(s.MCP["surface-web"].Args, "--viewport-size"); err != nil || got != "1280,800" {
			t.Errorf("--viewport-size = %q, %v", got, err)
		}
	})
	t.Run("mcp browser: a fresh profile per run", func(t *testing.T) {
		f := newLaunch(t)
		a, _ := f.launch(t)
		b, _ := f.launch(t)
		pa, pb := arg(a.MCP["surface-web"].Args, "--user-data-dir"), arg(b.MCP["surface-web"].Args, "--user-data-dir")
		if pa == "" || pa == pb {
			t.Errorf("profiles %q and %q", pa, pb)
		}
		if info, err := os.Stat(pa); err != nil || !info.IsDir() {
			t.Errorf("the profile %s was not made: %v", pa, err)
		}
	})
	t.Run("mcp browser: no display", func(t *testing.T) {
		f := newLaunch(t)
		f.l.Host.Env = []string{"PATH=" + os.Getenv("PATH")}
		if s, err := f.launch(t); s != nil || !isInfra(err, "no display") {
			t.Errorf("Launch() = %v, %v; want an Infra finding, never a headless round", s, err)
		}
	})
	// The bash case also warned on stderr that the endpoint was not up yet;
	// Go launches the app before the Step, so there is nothing to warn of.
	t.Run("mcp electron: registers anyway, warns", func(t *testing.T) {
		f := newLaunch(t)
		s, err := f.launch(t)
		bare := s.MCP["surface-bare"]
		if err != nil || !strings.HasPrefix(arg(bare.Args, "--cdp-endpoint"), "http://127.0.0.1:") {
			t.Errorf("surface-bare = %+v, %v", bare, err)
		}
	})
	t.Run("mcp: api is evidence-only", func(t *testing.T) {
		f := newLaunch(t)
		s, _ := f.launch(t, "api")
		if _, ok := s.MCP["surface-api"]; ok {
			t.Error("an api Surface got an MCP server")
		}
	})
	t.Run("mcp: unknown Surface", func(t *testing.T) {
		f := newLaunch(t)
		if s, err := f.launch(t, "nope"); s != nil || err == nil || !strings.Contains(err.Error(), `no Surface "nope"`) {
			t.Errorf("Launch() = %v, %v", s, err)
		}
	})
	t.Run("mcp-config: UI Surfaces only", func(t *testing.T) {
		f := newLaunch(t)
		s, _ := f.launch(t)
		var names []string
		for name := range s.MCP {
			names = append(names, name)
		}
		if got := strings.Join(slices.Sorted(slices.Values(names)), ","); got != "surface-bare,surface-panel,surface-web" {
			t.Errorf("servers = %s", got)
		}
	})
	t.Run("mcp-config: each entry runs this CLI for that Surface", func(t *testing.T) {
		f := newLaunch(t)
		s, _ := f.launch(t)
		for name, server := range s.MCP {
			if server.Command != "mcp-stub" {
				t.Errorf("%s runs %q", name, server.Command)
			}
		}
		if a, b := arg(s.MCP["surface-panel"].Args, "--cdp-endpoint"), arg(s.MCP["surface-bare"].Args, "--cdp-endpoint"); a == b {
			t.Errorf("two electron Surfaces share the endpoint %s", a)
		}
	})
	t.Run("mcp-config: no UI Surface", func(t *testing.T) {
		f := newLaunch(t)
		f.l.Host.Env = nil // no display is needed when nothing is drawn
		s, err := f.l.Launch(context.Background(), map[string]config.Surface{"api": {Kind: "api", URLKey: "API_URL"}}, []string{"api"}, launchBlock)
		if err != nil || len(s.MCP) != 0 {
			t.Errorf("Launch() = %+v, %v", s, err)
		}
	})
	t.Run("start: the app is launched wired to the environment", func(t *testing.T) {
		f := newLaunch(t)
		s, err := f.launch(t, "panel")
		rec, _ := os.ReadFile(f.record)
		endpoint := arg(s.MCP["surface-panel"].Args, "--cdp-endpoint")
		_, port, _ := strings.Cut(strings.TrimPrefix(endpoint, "http://"), ":")
		if err != nil || !strings.Contains(string(rec), "args: --remote-debugging-port="+port+"\n") ||
			!strings.Contains(string(rec), "PANEL_URL=http://panel.test") || !strings.Contains(string(rec), "DISPLAY=:99") {
			t.Errorf("err %v, the app recorded:\n%s", err, rec)
		}
	})
	t.Run("start: the sandbox line reports OK", func(t *testing.T) {
		f := newLaunch(t)
		s, err := f.launch(t, "panel")
		rec, _ := os.ReadFile(f.record)
		if err != nil || s.Sandbox != "" || !strings.Contains(string(rec), "SANDBOX_FLAG=unset") {
			t.Errorf("Sandbox = %q, %v; the app recorded:\n%s", s.Sandbox, err, rec)
		}
	})
	t.Run("stop: idempotent", func(t *testing.T) {
		f := newLaunch(t)
		s, err := f.launch(t, "panel")
		profile := arg(s.MCP["surface-web"].Args, "--user-data-dir")
		if err != nil {
			t.Fatal(err)
		}
		if e1, e2 := s.Close(), s.Close(); e1 != nil || e2 != nil {
			t.Errorf("Close() = %v, then %v", e1, e2)
		}
		for _, pid := range f.pids(t) {
			if alive(pid) {
				t.Errorf("the app (pid %d) is still running", pid)
			}
		}
		if _, err := os.Stat(profile); !os.IsNotExist(err) {
			t.Errorf("the profile %s is still there", profile)
		}
		if err := (*Session)(nil).Close(); err != nil {
			t.Errorf("closing a Session that never launched: %v", err)
		}
	})
	t.Run("start: DEGRADED is reported, never silent", func(t *testing.T) {
		f := newLaunch(t)
		f.l.Host.UsernsRestrictFile = write(t, "userns-on", "1\n")
		f.l.Host.AppArmorProfilesFile = write(t, "profiles-other", "/usr/bin/other (enforce)\n")
		s, err := f.launch(t, "panel")
		rec, _ := os.ReadFile(f.record)
		if err != nil || !strings.HasPrefix(s.Sandbox, "panel started with ELECTRON_DISABLE_SANDBOX=1") || !strings.Contains(string(rec), "SANDBOX_FLAG=1") {
			t.Errorf("Sandbox = %q, %v; the app recorded:\n%s", s.Sandbox, err, rec)
		}
	})
	t.Run("start: the endpoint never answered", func(t *testing.T) {
		f := newLaunch(t)
		f.l.ProbeCDP = func(context.Context, string) error { return errors.New("connection refused") }
		f.l.Wait = 100 * time.Millisecond
		s, err := f.launch(t, "panel")
		if s != nil || !isInfra(err, "did not come up after one retry") || !strings.Contains(err.Error(), "never answered") {
			t.Errorf("Launch() = %v, %v", s, err)
		}
		pids := f.pids(t)
		if len(pids) != 2 {
			t.Errorf("the app was started %d times, want 2 (one retry)", len(pids))
		}
		for _, pid := range pids {
			if alive(pid) {
				t.Errorf("the app (pid %d) was left running", pid)
			}
		}
		if profiles, _ := filepath.Glob(filepath.Join(f.l.RunDir, "profiles", "*")); len(profiles) != 0 {
			t.Errorf("a failed launch left profiles behind: %v", profiles)
		}
	})
	t.Run("start: an app that exits is retried once, then reported", func(t *testing.T) {
		f := newLaunch(t)
		f.l.ProbeCDP = func(context.Context, string) error { return errors.New("connection refused") }
		os.WriteFile(filepath.Join(f.l.Dir, "verify", "app"), []byte("#!/bin/sh\necho $$ >> \"$APP_RECORD.pids\"\nexit 1\n"), 0o755)
		if _, err := f.launch(t, "panel"); !isInfra(err, "exited") || len(f.pids(t)) != 2 {
			t.Errorf("err %v after %d starts", err, len(f.pids(t)))
		}
	})
	t.Run("log: a 25 MB stream leaves the app's log at its newest 10 MB", func(t *testing.T) {
		f := newLaunch(t)
		f.l.ProbeCDP = func(context.Context, string) error { return errors.New("connection refused") }
		// About 25 MB of numbered lines, on each of the two starts.
		os.WriteFile(filepath.Join(f.l.Dir, "verify", "app"), []byte("#!/bin/sh\necho $$ >> \"$APP_RECORD.pids\"\nseq 1 3500000\nexit 1\n"), 0o755)
		if _, err := f.launch(t, "panel"); !isInfra(err, "exited") {
			t.Fatalf("err = %v", err)
		}
		var stream strings.Builder
		for i := 1; i <= 3500000; i++ {
			stream.WriteString(strconv.Itoa(i) + "\n")
		}
		if stream.Len() < 25<<20 {
			t.Fatalf("the stream is %d bytes, under 25 MB", stream.Len())
		}
		log, err := os.ReadFile(filepath.Join(f.l.RunDir, "panel.log"))
		if err != nil {
			t.Fatal(err)
		}
		if len(log) != AppLogCap || AppLogCap != 10<<20 {
			t.Fatalf("the log is %d bytes, want 10 MB (%d)", len(log), AppLogCap)
		}
		if want := stream.String()[stream.Len()-AppLogCap:]; string(log) != want {
			t.Errorf("the log is not the stream's last 10 MB: it ends %q", log[len(log)-40:])
		}
	})
	t.Run("start: no launcher declared", func(t *testing.T) {
		f := newLaunch(t)
		if _, err := f.launch(t, "bare"); err == nil || !strings.Contains(err.Error(), "declares no launcher") {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("start: the url_key is missing", func(t *testing.T) {
		f := newLaunch(t)
		s, err := f.l.Launch(context.Background(), launchSurfaces, []string{"panel"}, [][2]string{{"WEB_URL", "http://web.test"}})
		if s != nil || err == nil || !strings.Contains(err.Error(), "PANEL_URL is not in the provider's up block") {
			t.Errorf("Launch() = %v, %v", s, err)
		}
	})
	t.Run("start: browser Surfaces are not launched", func(t *testing.T) {
		f := newLaunch(t)
		if s, err := f.launch(t, "web"); err != nil || len(s.apps) != 0 || len(f.pids(t)) != 0 {
			t.Errorf("Launch() = %+v, %v", s, err)
		}
	})
	t.Run("up: a browser Surface is not launched here", func(t *testing.T) {
		f := newLaunch(t)
		s, err := f.launch(t, "web")
		if err != nil || len(s.apps) != 0 || len(f.pids(t)) != 0 || s.Sandbox != "" {
			t.Errorf("Launch() = %+v, %v", s, err)
		}
		if _, ok := s.MCP["surface-web"]; !ok {
			t.Errorf("the browser Surface has no MCP server: %+v", s.MCP)
		}
	})
	t.Run("stop: the app's whole group, after the app itself exited", func(t *testing.T) {
		f := newLaunch(t)
		f.l.ProbeCDP = func(context.Context, string) error { return errors.New("connection refused") }
		os.WriteFile(filepath.Join(f.l.Dir, "verify", "app"), []byte("#!/bin/sh\nsleep 300 &\necho $! >> \"$APP_RECORD.pids\"\nexit 0\n"), 0o755)
		if _, err := f.launch(t, "panel"); !isInfra(err, "exited") {
			t.Errorf("err = %v", err)
		}
		pids := f.pids(t)
		if len(pids) != 2 {
			t.Fatalf("recorded %d background children, want 2", len(pids))
		}
		for _, pid := range pids {
			if alive(pid) {
				t.Errorf("the app's child (pid %d) outlived its launcher", pid)
			}
		}
	})
	// verify-boot: the environment is the caller's, so a failed launch tears
	// down only what Launch made and leaves the environment to it.
	t.Run("up: a failed launch leaves the environment up", func(t *testing.T) {
		f := newLaunch(t)
		f.l.ProbeCDP = func(context.Context, string) error { return errors.New("connection refused") }
		f.l.Wait = 50 * time.Millisecond
		if _, err := f.launch(t, "panel"); !isInfra(err, "panel") {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("up: the sandbox verdict is in the block", func(t *testing.T) {
		f := newLaunch(t)
		f.l.Host.Env = append(f.l.Host.Env, "AUTO_AGENT_ELECTRON_SANDBOX=disabled")
		s, err := f.launch(t, "panel")
		if err != nil {
			t.Fatal(err)
		}
		round := checklist.Round{N: 1, Max: 3, Sandbox: s.Sandbox}
		if !strings.Contains(checklist.Comment(round), "**Sandbox DEGRADED:** panel started with ELECTRON_DISABLE_SANDBOX=1") ||
			!strings.HasSuffix(round.Line(), "(sandbox DEGRADED)") {
			t.Errorf("the round's evidence does not carry the DEGRADED sandbox:\n%s\n%s", checklist.Comment(round), round.Line())
		}
	})
	t.Run("down: apps then environment, twice", func(t *testing.T) {
		f := newLaunch(t)
		s, _ := f.launch(t, "panel")
		s.Close()
		s.Close()
		for _, pid := range f.pids(t) {
			if alive(pid) {
				t.Errorf("the app (pid %d) is still running", pid)
			}
		}
	})
}
