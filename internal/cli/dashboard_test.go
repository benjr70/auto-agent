package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/benjr70/auto-agent/internal/github/githubtest"
	"github.com/benjr70/auto-agent/internal/step"
)

// freePort is a port nothing listens on, as far as the test can tell.
func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

// dashboardGet asks the Dashboard at host:port for path.
func dashboardGet(host, port, path string) (int, string, error) {
	client := http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get("http://" + net.JoinHostPort(host, port) + path)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body), err
}

// idleDaemon is a Daemon fixture whose cycles find no work, and that asks
// the Dashboard for each path while it sleeps after its first cycle.
func idleDaemon(t *testing.T, host string, paths ...string) (f *fireFixture, answers map[string]string, errs map[string]error) {
	f = newFireFixture(t, labelsConfig)
	f.allLabels()
	f.queue = []githubtest.Reply{queueReply()}
	answers, errs = map[string]string{}, map[string]error{}
	f.onSleep = func(time.Duration) {
		if len(answers)+len(errs) > 0 {
			return
		}
		for _, path := range paths {
			code, body, err := dashboardGet(host, f.environ["AUTO_AGENT_DASHBOARD_PORT"], path)
			if err != nil {
				errs[path] = err
				continue
			}
			answers[path] = strconv.Itoa(code) + " " + body
		}
	}
	return f, answers, errs
}

func TestRunServesTheDashboard(t *testing.T) {
	t.Run("test_binds_loopback_by_default", func(t *testing.T) {
		f, answers, errs := idleDaemon(t, "127.0.0.1", "/api/status", "/", "/nope")
		if code := f.daemon(1); code != 0 {
			t.Fatalf("exit %d, stderr %q", code, f.stderr.String())
		}
		if len(errs) != 0 {
			t.Fatalf("the Dashboard did not answer: %v (stderr %q)", errs, f.stderr.String())
		}
		status, ok := strings.CutPrefix(answers["/api/status"], "200 ")
		var got struct {
			Host struct {
				Bind string
				Port int
				Repo string
			}
			Fires struct{ Items []any }
		}
		if !ok || json.Unmarshal([]byte(status), &got) != nil {
			t.Fatalf("GET /api/status = %q", answers["/api/status"])
		}
		port, _ := strconv.Atoi(f.environ["AUTO_AGENT_DASHBOARD_PORT"])
		if got.Host.Bind != "127.0.0.1" || got.Host.Port != port || got.Host.Repo != "acme/widgets" {
			t.Errorf("host = %+v", got.Host)
		}
		if !strings.HasPrefix(answers["/"], "200 ") || !strings.HasPrefix(answers["/nope"], "404 ") {
			t.Errorf("GET / = %.40q, GET /nope = %.40q", answers["/"], answers["/nope"])
		}
		if want := fmt.Sprintf("the Dashboard is listening on http://127.0.0.1:%d/", port); !strings.Contains(f.stderr.String(), want) {
			t.Errorf("stderr %q, want %q", f.stderr.String(), want)
		}
		// The Daemon stopped, and the Dashboard with it.
		if _, _, err := dashboardGet("127.0.0.1", f.environ["AUTO_AGENT_DASHBOARD_PORT"], "/"); err == nil {
			t.Error("the Dashboard still answers after the Daemon stopped")
		}
	})

	t.Run("test_bind_and_port_from_the_host_env", func(t *testing.T) {
		f, answers, errs := idleDaemon(t, "127.0.0.2", "/api/status")
		f.environ["AUTO_AGENT_DASHBOARD_BIND"] = "127.0.0.2"
		var loopback error
		next := f.onSleep
		f.onSleep = func(d time.Duration) {
			next(d)
			_, _, loopback = dashboardGet("127.0.0.1", f.environ["AUTO_AGENT_DASHBOARD_PORT"], "/api/status")
		}
		if code := f.daemon(1); code != 0 {
			t.Fatalf("exit %d, stderr %q", code, f.stderr.String())
		}
		if len(errs) != 0 || !strings.HasPrefix(answers["/api/status"], "200 ") || !strings.Contains(answers["/api/status"], `"bind":"127.0.0.2"`) {
			t.Errorf("GET 127.0.0.2/api/status = %q, %v", answers["/api/status"], errs)
		}
		if loopback == nil {
			t.Error("the Dashboard bound to 127.0.0.2 answers on 127.0.0.1 too")
		}
	})

	t.Run("a port another process holds: the Daemon runs without a Dashboard, with a warning", func(t *testing.T) {
		f, answers, _ := idleDaemon(t, "127.0.0.1", "/api/status")
		held, err := net.Listen("tcp", "127.0.0.1:"+f.environ["AUTO_AGENT_DASHBOARD_PORT"])
		if err != nil {
			t.Fatal(err)
		}
		defer held.Close()
		if code := f.daemon(2); code != 0 {
			t.Fatalf("exit %d, stderr %q", code, f.stderr.String())
		}
		if records := f.logged(); len(records) != 2 || records[1]["kind"] != "idle" {
			t.Errorf("the Daemon logged %v, want its two cycles", records)
		}
		if !strings.Contains(f.stderr.String(), "warning: no Dashboard, the Daemon runs without one") {
			t.Errorf("stderr %q, want the warning", f.stderr.String())
		}
		if len(answers) != 0 {
			t.Errorf("something answered on the held port: %v", answers)
		}
	})
}

// dialDuringStep is a Step runner that, while each Step runs, tries the
// Dashboard's port.
type dialDuringStep struct {
	step.Runner
	port     string
	steps    int
	answered bool
}

func (r *dialDuringStep) Run(ctx context.Context, spec step.Spec, call step.Call) step.Run {
	r.steps++
	if _, _, err := dashboardGet("127.0.0.1", r.port, "/api/status"); err == nil {
		r.answered = true
	}
	return r.Runner.Run(ctx, spec, call)
}

func TestRunOnceServesNoDashboard(t *testing.T) {
	f := newFireFixture(t, labelsConfig)
	f.answer(finished)
	dial := &dialDuringStep{Runner: f.runner, port: f.environ["AUTO_AGENT_DASHBOARD_PORT"]}
	f.fixture.runner = dial
	if code := f.fire(); code != 0 {
		t.Fatalf("exit %d, stderr %q", code, f.stderr.String())
	}
	if dial.steps == 0 {
		t.Fatal("the Fire ran no Step to look from")
	}
	if dial.answered || strings.Contains(f.stderr.String(), "Dashboard") {
		t.Errorf("run --once served a Dashboard: answered %v, stderr %q", dial.answered, f.stderr.String())
	}
}
