package verify

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/benjr70/auto-agent/internal/config"
	"github.com/benjr70/auto-agent/internal/pidfile"
	"github.com/benjr70/auto-agent/internal/step"
)

// DefaultMCPCommand is the MCP server a UI Surface is driven through.
var DefaultMCPCommand = []string{"npx", "-y", "@playwright/mcp@latest"}

// The kinds of Surface: a browser and an Electron app are driven through an
// MCP server; a CLI and an API are evidence-only and have no launcher.
const (
	KindBrowser  = "browser"
	KindElectron = "electron"
)

const (
	// appWait is how long an app may take before its DevTools endpoint
	// answers; appInterval how often it is asked.
	appWait     = time.Minute
	appInterval = 500 * time.Millisecond
	// stopGrace is how long a stopped app has to exit before it is killed.
	stopGrace = 5 * time.Second
	// cdpProbeTimeout bounds one ask of a DevTools endpoint.
	cdpProbeTimeout = 2 * time.Second
)

// Launcher launches the Surfaces one Verify Step drives.
type Launcher struct {
	Host Host
	// Dir is the checkout: an electron Surface's launcher is relative to it,
	// and its app runs there.
	Dir string
	// RunDir holds the browser profiles, the apps' logs and their pid files.
	RunDir string
	// Tmp is the round's tmp directory: the apps and the MCP servers get it
	// as TMPDIR, so that nothing they write lands in the Host's /tmp. Empty
	// leaves them the Host's TMPDIR.
	Tmp string
	// MCPCommand is the MCP server's command; nil means DefaultMCPCommand.
	MCPCommand []string
	// ProbeCDP answers whether an app's DevTools endpoint is up; nil asks
	// its /json/version over HTTP.
	ProbeCDP func(ctx context.Context, endpoint string) error
	// Wait and Interval bound the wait for an app; zero means a minute,
	// asked every half second.
	Wait, Interval time.Duration
}

// Session is the launched Surfaces of one Verify Step: the apps, the browser
// profiles, and the MCP configuration that reaches them. Close tears it all
// down, on every exit.
type Session struct {
	// MCP is what the Step is handed: one server per UI Surface.
	MCP map[string]step.MCPServer
	// Sandbox is the detail of a DEGRADED app sandbox, for the round's
	// evidence; empty when every app ran sandboxed or none was launched.
	Sandbox string

	apps     []*app
	profiles []string
}

// app is one launched Electron app.
type app struct {
	cmd *exec.Cmd
	// pidFile names it, so that the Sweep can stop it when the Daemon was
	// killed before the app was.
	pidFile string
	exited  chan struct{}
	// logged is closed once everything the app and its group wrote is in
	// its log.
	logged chan struct{}
}

// Launch makes the MCP configuration for every UI Surface the head declares
// and launches the app of each electron Surface named in apps, with the
// provider's up block in its environment. A UI Surface needs the Host's
// display: none is an Infra finding, as is an app that does not come up
// after one retry. Whatever was launched is torn down when Launch fails.
func (l Launcher) Launch(ctx context.Context, surfaces map[string]config.Surface, apps []string, block [][2]string) (*Session, error) {
	s := &Session{MCP: map[string]step.MCPServer{}}
	for _, name := range apps {
		if _, ok := surfaces[name]; !ok {
			return nil, fmt.Errorf("no Surface %q is declared in the Harness config", name)
		}
	}
	var ui []string
	for _, name := range slices.Sorted(maps.Keys(surfaces)) {
		if kind := surfaces[name].Kind; kind == KindBrowser || kind == KindElectron {
			ui = append(ui, name)
		}
	}
	if len(ui) == 0 {
		return s, nil
	}
	display, err := l.Host.Display(ctx)
	if err != nil {
		return nil, err
	}
	command := l.MCPCommand
	if len(command) == 0 {
		command = DefaultMCPCommand
	}
	endpoints := map[string]string{}
	for _, name := range ui {
		surface := surfaces[name]
		args := append([]string{}, command[1:]...)
		switch surface.Kind {
		case KindBrowser:
			// A fresh profile per run: no cookie, permission or session from
			// an earlier round can change what this round sees.
			profiles := filepath.Join(l.RunDir, "profiles")
			if err := os.MkdirAll(profiles, 0o755); err != nil {
				s.Close()
				return nil, err
			}
			profile, err := os.MkdirTemp(profiles, name+"-")
			if err != nil {
				s.Close()
				return nil, err
			}
			s.profiles = append(s.profiles, profile)
			viewport := or(surface.Viewport, DefaultViewport)
			args = append(args, "--browser", "chrome", "--user-data-dir", profile, "--viewport-size", strings.Replace(viewport, "x", ",", 1))
		case KindElectron:
			// The server registers before the app exists and dials lazily,
			// on the first tool call.
			port, err := freePort()
			if err != nil {
				s.Close()
				return nil, err
			}
			endpoints[name] = "http://127.0.0.1:" + strconv.Itoa(port)
			args = append(args, "--cdp-endpoint", endpoints[name])
		}
		server := step.MCPServer{Command: command[0], Args: args, Env: map[string]string{"DISPLAY": display}}
		if l.Tmp != "" {
			server.Env["TMPDIR"] = l.Tmp
		}
		s.MCP["surface-"+name] = server
	}

	env := append(append([]string{}, l.Host.Env...), "DISPLAY="+display)
	for _, kv := range block {
		env = append(env, kv[0]+"="+kv[1])
	}
	if l.Tmp != "" {
		// The last of a duplicated key is the one the app sees.
		env = append(env, "TMPDIR="+l.Tmp)
	}
	for _, name := range apps {
		if surfaces[name].Kind != KindElectron {
			continue // only an electron Surface has an app to launch
		}
		if err := l.launchApp(ctx, s, name, surfaces[name], block, endpoints[name], env); err != nil {
			s.Close()
			return nil, err
		}
	}
	return s, nil
}

// launchApp launches one electron Surface's app and waits for its DevTools
// endpoint, retrying a failed launch once.
func (l Launcher) launchApp(ctx context.Context, s *Session, name string, surface config.Surface, block [][2]string, endpoint string, env []string) error {
	if surface.Launcher == "" {
		return fmt.Errorf("electron Surface %s declares no launcher; the harness does not know how to open this project's app", name)
	}
	carried := false
	for _, kv := range block {
		carried = carried || kv[0] == surface.URLKey
	}
	if surface.URLKey != "" && !carried {
		return fmt.Errorf("%s is not in the provider's up block, and the app of Surface %s is launched pointed at it", surface.URLKey, name)
	}
	bin := surface.Launcher
	if !filepath.IsAbs(bin) {
		bin = filepath.Join(l.Dir, bin)
	}
	if info, err := os.Stat(bin); err != nil || !info.Mode().IsRegular() || info.Mode()&0o111 == 0 {
		return fmt.Errorf("the launcher %s of Surface %s is not an executable file under %s", surface.Launcher, name, l.Dir)
	}
	mode := l.Host.SandboxMode(bin)
	if mode.Degraded() {
		env = append(env, "ELECTRON_DISABLE_SANDBOX=1")
	}
	var failures []string
	for attempt := 0; attempt < 2; attempt++ {
		a, err := l.startApp(ctx, name, bin, endpoint, env)
		if err == nil {
			s.apps = append(s.apps, a)
			if mode.Degraded() {
				s.Sandbox = fmt.Sprintf("%s started with ELECTRON_DISABLE_SANDBOX=1 (no AppArmor profile grants user namespaces to %s on this Host)", name, surface.Launcher)
			}
			return nil
		}
		failures = append(failures, err.Error())
		if ctx.Err() != nil {
			break
		}
	}
	return &Infra{Reason: fmt.Sprintf("the app of Surface %s did not come up after one retry: %s (log %s)", name, strings.Join(failures, "; then "), l.logPath(name))}
}

func (l Launcher) logPath(name string) string { return filepath.Join(l.RunDir, name+".log") }

// startApp starts the app once and waits for its DevTools endpoint. An app
// that does not answer is stopped, so a failed start leaves nothing behind.
func (l Launcher) startApp(ctx context.Context, name, bin, endpoint string, env []string) (*app, error) {
	if err := os.MkdirAll(l.RunDir, 0o755); err != nil {
		return nil, err
	}
	// The app's own output goes to its log, never into the Daemon's, and
	// only its last AppLogCap bytes are kept. The log is fed through a pipe
	// rather than handed to the app, so that it can be cut back.
	log, err := openAppLog(l.logPath(name))
	if err != nil {
		return nil, err
	}
	r, w, err := os.Pipe()
	if err != nil {
		log.Close()
		return nil, err
	}
	_, port, _ := strings.Cut(strings.TrimPrefix(endpoint, "http://"), ":")
	cmd := exec.Command(bin, "--remote-debugging-port="+port)
	cmd.Dir, cmd.Env, cmd.Stdout, cmd.Stderr = l.Dir, env, w, w
	// The app's children (a renderer, a GPU process) die with it.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	err = cmd.Start()
	w.Close()
	if err != nil {
		r.Close()
		log.Close()
		return nil, fmt.Errorf("starting %s: %v", bin, err)
	}
	a := &app{cmd: cmd, pidFile: filepath.Join(l.RunDir, name+".pid"), exited: make(chan struct{}), logged: make(chan struct{})}
	go copyAppLog(r, log, a.logged)
	go func() { cmd.Wait(); close(a.exited) }()
	// An app already gone has nothing to record, and the wait below says
	// why it went.
	if err := pidfile.Write(a.pidFile, cmd.Process.Pid); err != nil && syscall.Kill(cmd.Process.Pid, 0) == nil {
		a.stop()
		return nil, fmt.Errorf("writing the pid file of %s: %v", bin, err)
	}

	probe := l.ProbeCDP
	if probe == nil {
		probe = askCDP
	}
	wait, interval := l.Wait, l.Interval
	if wait <= 0 {
		wait = appWait
	}
	if interval <= 0 {
		interval = appInterval
	}
	deadline := time.Now().Add(wait)
	for {
		if probe(ctx, endpoint) == nil {
			return a, nil
		}
		var why string
		select {
		case <-a.exited:
			why = fmt.Sprintf("it exited (%s) before its DevTools endpoint answered", cmd.ProcessState)
		case <-ctx.Done():
			why = ctx.Err().Error()
		case <-time.After(interval):
			if time.Now().Before(deadline) {
				continue
			}
			why = fmt.Sprintf("its DevTools endpoint at %s never answered within %s", endpoint, wait)
		}
		a.stop()
		return nil, errors.New(why)
	}
}

// stop ends the app and everything it started, and waits for them. The
// whole process group is signalled even when the app itself has already
// exited: a launcher that backgrounds the real app, or an app whose main
// process died first, leaves the rest of its group running otherwise.
func (a *app) stop() {
	defer os.Remove(a.pidFile)
	group := -a.cmd.Process.Pid
	syscall.Kill(group, syscall.SIGTERM)
	grace := time.After(stopGrace)
	// Its log is complete once the group is gone, unless something it
	// started left the group and still holds the pipe.
	defer func() {
		select {
		case <-a.logged:
		case <-time.After(stopGrace):
		}
	}()
	select {
	case <-a.exited:
	case <-grace:
		syscall.Kill(group, syscall.SIGKILL)
		<-a.exited
		return
	}
	// The leader is reaped; wait out the rest of its group.
	for groupAlive(group) {
		select {
		case <-grace:
			syscall.Kill(group, syscall.SIGKILL)
			return
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// groupAlive reports whether any process in the group is still there.
func groupAlive(group int) bool {
	return syscall.Kill(group, 0) == nil
}

// Close stops every launched app and removes every browser profile. It is
// idempotent, and safe on a Session that launched nothing.
func (s *Session) Close() error {
	if s == nil {
		return nil
	}
	var errs []error
	for _, a := range s.apps {
		a.stop()
	}
	for _, profile := range s.profiles {
		if err := os.RemoveAll(profile); err != nil {
			errs = append(errs, err)
		}
	}
	s.apps, s.profiles = nil, nil
	return errors.Join(errs...)
}

// askCDP is the default DevTools probe.
func askCDP(ctx context.Context, endpoint string) error {
	ctx, cancel := context.WithTimeout(ctx, cdpProbeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"/json/version", nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s/json/version answered %s", endpoint, resp.Status)
	}
	return nil
}

// freePort is a local port nothing listens on now.
func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}
