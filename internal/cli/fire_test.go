package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/benjr70/auto-agent/internal/config"
	"github.com/benjr70/auto-agent/internal/github/githubtest"
	"github.com/benjr70/auto-agent/internal/step"
	"github.com/benjr70/auto-agent/internal/step/implement"
	"github.com/benjr70/auto-agent/internal/step/steptest"
)

var fireClock = time.Date(2026, 10, 12, 9, 30, 0, 0, time.UTC)

const (
	ticketBody = "## Acceptance criteria\n\n- [ ] it greets"
	lockPath   = "/repos/acme/widgets/issues/20/labels"
	unlockPath = "/repos/acme/widgets/issues/20/labels/AFK:in-progress"
	pullsPath  = "/repos/acme/widgets/pulls"
)

// git runs git for the test's own setup, away from the user's config.
func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=Upstream", "GIT_AUTHOR_EMAIL=up@example.com", "GIT_COMMITTER_NAME=Upstream", "GIT_COMMITTER_EMAIL=up@example.com")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// countingRunner notes how many write requests GitHub had received when the
// Step started, then runs it.
type countingRunner struct {
	step.Runner
	gh           *githubtest.Server
	writesBefore []githubtest.Request
}

func (r *countingRunner) Run(ctx context.Context, spec step.Spec, call step.Call) step.Run {
	r.writesBefore = r.gh.Writes()
	return r.Runner.Run(ctx, spec, call)
}

// interruptedRunner is a Step during which the Daemon is told to stop.
type interruptedRunner struct{}

func (interruptedRunner) Run(ctx context.Context, _ step.Spec, _ step.Call) step.Run {
	syscall.Kill(os.Getpid(), syscall.SIGTERM)
	<-ctx.Done()
	return step.Run{Outcome: step.Failed, Detail: "the caller gave up"}
}

// fireFixture is a whole Fire's world: the Target Project as a bare remote
// with a Harness config on trunk, a fake GitHub that has one eligible ticket
// (#20), a fake claude, and a State dir.
type fireFixture struct {
	*fixture
	t      *testing.T
	remote string
	state  string
	out    string // where the fake Step leaves what it saw
	claude *steptest.Fake
	runner *countingRunner
	// queue is what GitHub answers the queue query with, Fire by Fire.
	queue  []githubtest.Reply
	queued bool
}

func newFireFixture(t *testing.T, harness string) *fireFixture {
	t.Helper()
	root := t.TempDir()
	f := &fireFixture{fixture: newFixture(t, harness), t: t, remote: filepath.Join(root, "remote.git"), state: filepath.Join(root, "state"), out: filepath.Join(root, "out"), claude: steptest.New(t)}
	os.MkdirAll(f.out, 0o755)
	git(t, root, "init", "--quiet", "--bare", "--initial-branch=trunk", f.remote)
	// The fixture's checkout, with its Harness config, becomes trunk.
	git(t, f.dir, "init", "--quiet", "--initial-branch=trunk")
	git(t, f.dir, "add", "-A")
	git(t, f.dir, "commit", "--quiet", "-m", "first")
	git(t, f.dir, "push", "--quiet", f.remote, "HEAD:trunk")

	f.environ[EnvGitRemote] = f.remote
	f.environ["AUTO_AGENT_STATE_DIR"] = f.state
	f.runner = &countingRunner{Runner: &step.Claude{Binary: f.claude.Path}, gh: f.gh}
	f.fixture.runner = f.runner

	f.queue = []githubtest.Reply{queueReply(cleanSlice)}
	f.gh.Handle("GET", "/repos/acme/widgets/issues/20", githubtest.JSON(200, map[string]any{"number": 20, "title": "Tracer", "body": ticketBody, "state": "open"}))
	f.gh.Handle("POST", lockPath, githubtest.Text(200, `[]`))
	f.gh.Handle("DELETE", unlockPath, githubtest.Text(200, `[]`))
	f.gh.Handle("POST", pullsPath, githubtest.JSON(201, map[string]any{"number": 55, "head": map[string]any{"ref": "feat/issue-20"}}))
	return f
}

// answer scripts the fake Step: it edits the checkout, notes the branch it
// was on and the Fire record as it stood, and returns the result.
func (f *fireFixture) answer(result any) {
	f.claude.Script(steptest.Reply{
		Run: fmt.Sprintf("echo hello > greeting.txt; git rev-parse --abbrev-ref HEAD > '%s/branch'; cp '%s'/fires/*.json '%s/in-flight.json'", f.out, f.state, f.out),
		Lines: []string{
			steptest.Init("session-1", "claude-opus", append([]string{"StructuredOutput"}, implement.Spec().Tools...), []string{"factory:implement"}),
			steptest.Result(map[string]any{"structured_output": result, "num_turns": 9, "total_cost_usd": 1.25, "usage": map[string]any{"input_tokens": 100, "output_tokens": 20}}),
		},
	})
}

var finished = implement.Result{
	Finished: true, Summary: "Adds the greeting.", CommitMessage: "feat(a): add the greeting\n\nIt says hello.",
	Checklist: []string{"Run it and see hello", "Run it twice and see hello twice"},
}

func (f *fireFixture) fire(args ...string) int {
	if !f.queued {
		f.gh.HandleGraphQL("issues(first: 100", f.queue...)
		f.queued = true
	}
	if len(args) == 0 {
		args = []string{"--repo", "acme/widgets"}
	}
	return f.run(append([]string{"run", "--once"}, args...)...)
}

// record is the one Fire record in the State dir.
func (f *fireFixture) record() map[string]any {
	f.t.Helper()
	files, _ := filepath.Glob(filepath.Join(f.state, "fires", "*.json"))
	if len(files) != 1 {
		f.t.Fatalf("%d Fire records, want 1 (stderr: %s)", len(files), f.stderr.String())
	}
	return readJSON(f.t, files[0])
}

func readJSON(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var v map[string]any
	if err := json.Unmarshal(data, &v); err != nil {
		t.Fatalf("%s is not JSON: %v\n%s", path, err, data)
	}
	return v
}

func (f *fireFixture) writes() string {
	var lines []string
	for _, r := range f.gh.Writes() {
		lines = append(lines, r.Method+" "+r.Path)
	}
	return strings.Join(lines, "\n")
}

func (f *fireFixture) remoteRef(ref string) string {
	out, _ := exec.Command("git", "-C", f.remote, "rev-parse", "--verify", "--quiet", ref).Output()
	return strings.TrimSpace(string(out))
}

func TestFire(t *testing.T) {
	t.Run("a whole Fire: lock on, Step, push, PR opened, lock off, in that order", func(t *testing.T) {
		f := newFireFixture(t, labelsConfig)
		f.answer(finished)
		if code := f.fire(); code != 0 {
			t.Fatalf("exit %d, stderr %q, stdout %s", code, f.stderr.String(), f.stdout.String())
		}
		if want := "POST " + lockPath + "\nPOST " + pullsPath + "\nDELETE " + unlockPath; f.writes() != want {
			t.Fatalf("GitHub received\n%s\nwant\n%s", f.writes(), want)
		}
		// Every request, reads included, in the order GitHub received them.
		var all []string
		for _, r := range f.gh.Requests() {
			all = append(all, r.Method+" "+r.Path)
		}
		wantAll := []string{
			"GET /repos/acme/widgets", "GET /user",
			"GET /repos/acme/widgets/issues", "POST /graphql", "GET " + pullsPath,
			"GET /repos/acme/widgets/issues/20", "POST " + lockPath, "POST " + pullsPath, "DELETE " + unlockPath,
		}
		if strings.Join(all, "\n") != strings.Join(wantAll, "\n") {
			t.Errorf("GitHub received\n%s\nwant\n%s", strings.Join(all, "\n"), strings.Join(wantAll, "\n"))
		}
		writes := f.gh.Writes()
		if writes[0].Body != `{"labels":["AFK:in-progress"]}` {
			t.Errorf("the lock request carried %s", writes[0].Body)
		}
		if got := f.runner.writesBefore; len(got) != 1 || got[0].Path != lockPath {
			t.Errorf("when the Step started GitHub had received %+v, want only the lock", got)
		}

		// The Step ran in the Daemon's clone, on the ticket's branch.
		calls := f.claude.Calls()
		if len(calls) != 1 || calls[0].Dir != filepath.Join(f.state, "clone") {
			t.Fatalf("calls = %+v", calls)
		}
		if branch, _ := os.ReadFile(filepath.Join(f.out, "branch")); strings.TrimSpace(string(branch)) != "feat/issue-20" {
			t.Errorf("the Step ran on %q", branch)
		}
		wantInput := `/factory:implement {"issue":{"number":20,"title":"Tracer","body":"## Acceptance criteria\n\n- [ ] it greets"},"branch":"feat/issue-20","commands":{"install":"x","test":"y"},"commit_scopes":["a"]}`
		if calls[0].Stdin != wantInput {
			t.Errorf("the Step was sent\n%s\nwant\n%s", calls[0].Stdin, wantInput)
		}
		if model, _ := calls[0].Arg("--model"); model != "opus" {
			t.Errorf("the Step ran on %q, want the Host config's primary model", model)
		}

		// Go committed and pushed: the remote has the branch, one commit on trunk.
		head := f.remoteRef("refs/heads/feat/issue-20")
		if head == "" || git(t, f.remote, "rev-parse", head+"^") != f.remoteRef("refs/heads/trunk") {
			t.Fatalf("the remote's branch is %q, want one commit on trunk", head)
		}
		if got := git(t, f.remote, "log", "-1", "--format=%an <%ae>%n%B", head); got != "machine-user <machine-user@users.noreply.github.com>\nfeat(a): add the greeting\n\nIt says hello.\n\nCloses #20" {
			t.Errorf("the commit is\n%s", got)
		}
		if got := git(t, f.remote, "show", head+":greeting.txt"); got != "hello" {
			t.Errorf("greeting.txt = %q", got)
		}

		var pr map[string]string
		json.Unmarshal([]byte(writes[1].Body), &pr)
		wantBody := "## Summary\n\nAdds the greeting.\n\n## Closes\n\nCloses #20 — Tracer\n\n## Smoke\n\n`smoke: SKIPPED — this build does not run the smoke yet`\n\n" +
			"## Manual verification\n\n- [ ] Run it and see hello\n- [ ] Run it twice and see hello twice\n\n---\n\nGenerated by `auto-agent` at 2026-10-12T09:30:00Z\n"
		if pr["title"] != "feat(a): add the greeting" || pr["head"] != "feat/issue-20" || pr["base"] != "trunk" || pr["body"] != wantBody {
			t.Errorf("the pull request was %q %s -> %s\n%s\nwant body\n%s", pr["title"], pr["head"], pr["base"], pr["body"], wantBody)
		}

		// The Fire record lists the ticket and the Step.
		record := f.record()
		steps := record["steps"].([]any)
		if record["issue"] != 20.0 || record["pr"] != 55.0 || record["exit"] != 0.0 || record["kind"] != "pick" || record["phase"] != "done" ||
			record["branch"] != "feat/issue-20" || record["commit"] != head || record["error"] != "" || len(steps) != 1 {
			t.Fatalf("record = %v", record)
		}
		got, _ := json.Marshal(steps[0])
		for _, want := range []string{`"name":"implement"`, `"outcome":"OK"`, `"sessionId":"session-1"`, `"usage":{"input_tokens":100,"output_tokens":20}`, `"numTurns":9`, `"costUsd":1.25`, `"model":"claude-opus"`} {
			if !strings.Contains(string(got), want) {
				t.Errorf("the Step entry lacks %s: %s", want, got)
			}
		}
		var printed map[string]any
		if err := json.Unmarshal(f.stdout.Bytes(), &printed); err != nil || printed["fireId"] != record["fireId"] || printed["pr"] != 55.0 {
			t.Errorf("stdout is not the record: %v\n%s", err, f.stdout.String())
		}
	})

	// Ported from lib/fire.test.sh: test_in_flight_record_while_claude_runs.
	t.Run("the record is on disk while claude runs, in flight, and the final write replaces it", func(t *testing.T) {
		f := newFireFixture(t, labelsConfig)
		f.answer(finished)
		f.fire()
		during := readJSON(t, filepath.Join(f.out, "in-flight.json"))
		if during["endedAt"] != nil || during["exit"] != nil || during["issue"] != 20.0 || during["phase"] != "implement" || during["branch"] != "feat/issue-20" {
			t.Errorf("the record while the Step ran = %v", during)
		}
		after := f.record()
		if after["endedAt"] == nil || after["exit"] != 0.0 || after["fireId"] != during["fireId"] {
			t.Errorf("the final record = %v", after)
		}
	})

	// Ported from lib/fire.test.sh: test_pickup_fire_resets_the_checkout_first.
	t.Run("a plain Fire puts the checkout on the tip of the detected default branch before the skill runs", func(t *testing.T) {
		f := newFireFixture(t, labelsConfig)
		f.answer(finished)
		// What an earlier Fire that died left in the clone.
		f.queue = []githubtest.Reply{queueReply(), queueReply(cleanSlice)}
		f.fire()
		clone := filepath.Join(f.state, "clone")
		os.WriteFile(filepath.Join(clone, "stray.txt"), []byte("junk"), 0o644)
		// Trunk moves on.
		os.WriteFile(filepath.Join(f.dir, "NEW.md"), []byte("new\n"), 0o644)
		git(t, f.dir, "add", "-A")
		git(t, f.dir, "commit", "--quiet", "-m", "second")
		git(t, f.dir, "push", "--quiet", f.remote, "HEAD:trunk")
		if code := f.fire(); code != 0 {
			t.Fatalf("exit %d, stderr %q", code, f.stderr.String())
		}
		head := f.remoteRef("refs/heads/feat/issue-20")
		if git(t, f.remote, "rev-parse", head+"^") != f.remoteRef("refs/heads/trunk") {
			t.Error("the branch does not start at the remote's new trunk")
		}
		if files := git(t, f.remote, "ls-tree", "--name-only", "-r", head); strings.Contains(files, "stray.txt") || !strings.Contains(files, "NEW.md") {
			t.Errorf("the commit holds %q", files)
		}
	})
}

func TestFireStepNotOK(t *testing.T) {
	stream := func(lines ...string) []string {
		return append([]string{steptest.Init("session-9", "claude-opus", append([]string{"StructuredOutput"}, implement.Spec().Tools...), []string{"factory:implement"})}, lines...)
	}
	edit := "echo half > greeting.txt"
	tests := []struct {
		name    string
		reply   steptest.Reply
		outcome string
		detail  string
	}{
		{
			// Ported from lib/fire.test.sh: test_crashed_pick_clears_its_lock.
			// The bash wrapper flipped the lock to AFK:failed and commented;
			// here the lock comes off and nothing is posted, and the ticket
			// is the next Fire's.
			name:    "a crashed pick Fire clears the lock it took",
			reply:   steptest.Reply{Run: edit, Lines: stream(), Stderr: "segfault", ExitCode: 7},
			outcome: "FAILED", detail: "no result message (exit 7)",
		},
		{
			// Ported from lib/fire.test.sh: test_exhausted_pick_pauses. The
			// pause label and the wip commit are the pause Slice's.
			name:    "an EXHAUSTED pick Fire releases its lock and opens no PR",
			reply:   steptest.Reply{Run: edit, Lines: stream(), Stderr: "You've hit your session limit · resets 10:50pm (America/New_York)", ExitCode: 1},
			outcome: "EXHAUSTED", detail: "hit your session limit",
		},
		{
			// Ported from lib/fire.test.sh: test_rejected_event_is_the_outcome.
			name:    "a rejected rate-limit event in this Fire's stream is the exhaustion signal",
			reply:   steptest.Reply{Run: edit, Lines: stream(steptest.RateLimit("rejected", 1789491600, "five_hour")), ExitCode: 1},
			outcome: "EXHAUSTED", detail: "rate-limit event was rejected",
		},
		{
			// Ported from lib/fire.test.sh: test_auth_dead_fire_pauses_and_parks.
			// Parking is the Parked Slice's.
			name:    "an AUTH_DEAD Fire releases its lock and opens no PR",
			reply:   steptest.Reply{Lines: stream(), Stderr: "Failed to authenticate: OAuth session expired", ExitCode: 1},
			outcome: "AUTH_DEAD", detail: "Failed to authenticate",
		},
		{
			name:    "a result that fails the Implement check is MALFORMED: an unfinished result with no summary",
			reply:   steptest.Reply{Run: edit, Lines: stream(steptest.Success(implement.Result{Summary: " ", Checklist: []string{}}))},
			outcome: "MALFORMED", detail: "the summary is empty",
		},
		{
			name: "a finished result whose commit scope is not the Target Project's is MALFORMED",
			reply: steptest.Reply{Run: edit, Lines: stream(steptest.Success(implement.Result{
				Finished: true, Summary: "s", CommitMessage: "feat(nope): x", Checklist: []string{"c"},
			}))},
			outcome: "MALFORMED", detail: `scope "nope"`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFireFixture(t, labelsConfig)
			f.claude.Script(tt.reply)
			if code := f.fire(); code != 1 {
				t.Fatalf("exit %d, want 1 (stderr %q)", code, f.stderr.String())
			}
			// No PR, no comment, no other label: the lock on, the lock off.
			if want := "POST " + lockPath + "\nDELETE " + unlockPath; f.writes() != want {
				t.Errorf("GitHub received\n%s\nwant\n%s", f.writes(), want)
			}
			if head := f.remoteRef("refs/heads/feat/issue-20"); head != "" {
				t.Errorf("the remote got a branch at %s", head)
			}
			// A failed Fire still writes a Fire record
			// (lib/fire.test.sh: test_failed_fire_still_gets_a_record).
			record := f.record()
			entry := record["steps"].([]any)[0].(map[string]any)
			if record["exit"] != 1.0 || record["issue"] != 20.0 || record["pr"] != nil || record["phase"] != "implement" ||
				entry["outcome"] != tt.outcome || !strings.Contains(entry["detail"].(string), tt.detail) || entry["sessionId"] != "session-9" {
				t.Errorf("record = %v", record)
			}
			if !strings.Contains(record["error"].(string), "the implement Step ended "+tt.outcome) {
				t.Errorf("error = %q", record["error"])
			}
		})
	}
}

func TestFireEdges(t *testing.T) {
	t.Run("an unfinished Step's work is pushed for the next Fire, and no PR is opened", func(t *testing.T) {
		f := newFireFixture(t, labelsConfig)
		f.answer(implement.Result{Summary: "The parser is done; the printer remains.", Checklist: []string{}})
		if code := f.fire(); code != 1 {
			t.Fatalf("exit %d, want 1", code)
		}
		if want := "POST " + lockPath + "\nDELETE " + unlockPath; f.writes() != want {
			t.Errorf("GitHub received\n%s", f.writes())
		}
		head := f.remoteRef("refs/heads/feat/issue-20")
		if head == "" || git(t, f.remote, "log", "-1", "--format=%B", head) != "wip: partial work on #20\n\nThe parser is done; the printer remains." {
			t.Errorf("the remote's branch is %q", head)
		}
		record := f.record()
		if record["pr"] != nil || record["commit"] != head || !strings.Contains(record["error"].(string), "did not finish: The parser is done") {
			t.Errorf("record = %v", record)
		}
	})

	t.Run("a PR GitHub refuses leaves the work pushed and the lock released", func(t *testing.T) {
		f := newFireFixture(t, labelsConfig)
		f.answer(finished)
		f.gh.Handle("POST", pullsPath, githubtest.JSON(422, map[string]any{"message": "Validation Failed"}))
		if code := f.fire(); code != 1 {
			t.Fatalf("exit %d, want 1", code)
		}
		if want := "POST " + lockPath + "\nPOST " + pullsPath + "\nDELETE " + unlockPath; f.writes() != want {
			t.Errorf("GitHub received\n%s", f.writes())
		}
		record := f.record()
		if record["phase"] != "pr" || record["commit"] != f.remoteRef("refs/heads/feat/issue-20") || !strings.Contains(record["error"].(string), "Validation Failed") {
			t.Errorf("record = %v", record)
		}
	})

	t.Run("a lock that cannot be released fails the Fire and says so", func(t *testing.T) {
		f := newFireFixture(t, labelsConfig)
		f.answer(finished)
		f.gh.Handle("DELETE", unlockPath, githubtest.Text(500, `{"message":"boom"}`))
		if code := f.fire(); code != 1 {
			t.Fatalf("exit %d, want 1", code)
		}
		if record := f.record(); record["pr"] != 55.0 || !strings.Contains(record["error"].(string), "releasing the Single-flight lock on #20") {
			t.Errorf("record = %v", record)
		}
	})

	t.Run("a Fire interrupted while the Step runs still releases its lock", func(t *testing.T) {
		f := newFireFixture(t, labelsConfig)
		f.fixture.runner = interruptedRunner{}
		if code := f.fire(); code != 1 {
			t.Fatalf("exit %d, want 1", code)
		}
		if want := "POST " + lockPath + "\nDELETE " + unlockPath; f.writes() != want {
			t.Errorf("GitHub received\n%s\nwant\n%s", f.writes(), want)
		}
	})

	t.Run("a Target Project that was renamed is known by the name GitHub gives it now", func(t *testing.T) {
		f := newFireFixture(t, labelsConfig)
		f.queue = []githubtest.Reply{queueReply()}
		f.gh.Handle("GET", "/repos/acme/old-name", githubtest.JSON(200, map[string]string{"full_name": "acme/widgets", "default_branch": "trunk"}))
		if code := f.fire("--repo", "acme/old-name"); code != 0 {
			t.Fatalf("exit %d, stderr %q", code, f.stderr.String())
		}
		if record := f.record(); record["repo"] != "acme/widgets" {
			t.Errorf("repo = %v", record["repo"])
		}
	})

	// Ported from lib/fire.test.sh: test_failed_fire_still_gets_a_record.
	t.Run("a failed Fire still writes a Fire record", func(t *testing.T) {
		f := newFireFixture(t, labelsConfig)
		f.claude.Script(steptest.Reply{ExitCode: 7}) // no stream at all
		if code := f.fire(); code != 1 {
			t.Fatalf("exit %d, want 1", code)
		}
		record := f.record()
		entry := record["steps"].([]any)[0].(map[string]any)
		if record["exit"] != 1.0 || record["endedAt"] == nil || record["issue"] != 20.0 || entry["outcome"] != "FAILED" || entry["sessionId"] != "" || entry["usage"] != nil {
			t.Errorf("record = %v", record)
		}
		if !strings.Contains(f.stderr.String(), "the implement Step ended FAILED") {
			t.Errorf("stderr = %q", f.stderr.String())
		}
	})

	t.Run("a Step that leaves the clone on another branch has nothing committed, and the lock is released", func(t *testing.T) {
		f := newFireFixture(t, labelsConfig)
		f.answer(finished)
		calls := filepath.Join(f.out, "moved")
		f.claude.Script(steptest.Reply{
			Run: "git checkout --quiet -b elsewhere; echo x > x.txt; touch '" + calls + "'",
			Lines: []string{
				steptest.Init("s", "m", append([]string{"StructuredOutput"}, implement.Spec().Tools...), []string{"factory:implement"}),
				steptest.Success(finished),
			},
		})
		if code := f.fire(); code != 1 {
			t.Fatalf("exit %d, want 1", code)
		}
		if want := "POST " + lockPath + "\nDELETE " + unlockPath; f.writes() != want {
			t.Errorf("GitHub received\n%s", f.writes())
		}
		if record := f.record(); record["phase"] != "commit" || !strings.Contains(record["error"].(string), "not on feat/issue-20") || f.remoteRef("refs/heads/elsewhere") != "" {
			t.Errorf("record = %v", record)
		}
	})

	t.Run("a lock that cannot be applied starts no Step", func(t *testing.T) {
		f := newFireFixture(t, labelsConfig)
		f.gh.Handle("POST", lockPath, githubtest.Text(403, `{"message":"Resource not accessible"}`))
		if code := f.fire(); code != 1 || len(f.claude.Calls()) != 0 {
			t.Fatalf("exit %d, %d Step calls", code, len(f.claude.Calls()))
		}
		if f.writes() != "POST "+lockPath {
			t.Errorf("GitHub received\n%s", f.writes())
		}
	})

	// Ported from lib/fire.test.sh: test_success_and_nothing_picked_never_touch_a_lock.
	t.Run("a Fire that picked nothing writes no label", func(t *testing.T) {
		for name, prepare := range map[string]func(*fireFixture){
			"an empty queue": func(f *fireFixture) { f.queue = []githubtest.Reply{queueReply()} },
			"a held lock": func(f *fireFixture) {
				f.gh.Handle("GET", "/repos/acme/widgets/issues", githubtest.Text(200, `[{"number":9}]`))
			},
			"a ticket whose Agent PR is already open": func(f *fireFixture) {
				f.gh.Handle("GET", pullsPath, githubtest.Text(200, `[{"number":55,"head":{"ref":"feat/issue-20","repo":{"full_name":"acme/widgets"}}}]`))
			},
		} {
			t.Run(name, func(t *testing.T) {
				f := newFireFixture(t, labelsConfig)
				prepare(f)
				if code := f.fire(); code != 0 {
					t.Fatalf("exit %d, stderr %q", code, f.stderr.String())
				}
				if f.writes() != "" || len(f.claude.Calls()) != 0 {
					t.Errorf("GitHub received %q; %d Step calls", f.writes(), len(f.claude.Calls()))
				}
				if record := f.record(); record["issue"] != nil || record["exit"] != 0.0 || record["phase"] != "done" || len(record["steps"].([]any)) != 0 {
					t.Errorf("record = %v", record)
				}
			})
		}
	})

	// Ported from lib/fire.test.sh: test_preflight_failure_writes_record_and_skips_claude.
	t.Run("an invalid Harness config fails closed before claude runs, with a record", func(t *testing.T) {
		f := newFireFixture(t, labelsConfig)
		os.WriteFile(filepath.Join(f.dir, ".auto-agent", "harness.json"), []byte(`{"commit_scopes": [], "commands": {}}`), 0o644)
		git(t, f.dir, "commit", "--quiet", "-am", "break the config")
		git(t, f.dir, "push", "--quiet", f.remote, "HEAD:trunk")
		if code := f.fire(); code != 1 || len(f.claude.Calls()) != 0 || f.writes() != "" {
			t.Fatalf("exit %d, %d Step calls, writes %q", code, len(f.claude.Calls()), f.writes())
		}
		record := f.record()
		if record["phase"] != "preflight" || record["exit"] != 1.0 || !strings.Contains(record["error"].(string), "commands") {
			t.Errorf("record = %v", record)
		}
	})

	t.Run("a remote that cannot be cloned is a preflight failure with a record", func(t *testing.T) {
		f := newFireFixture(t, labelsConfig)
		f.environ[EnvGitRemote] = filepath.Join(t.TempDir(), "nowhere.git")
		if code := f.fire(); code != 1 || len(f.claude.Calls()) != 0 || f.writes() != "" {
			t.Fatalf("exit %d, %d Step calls, writes %q", code, len(f.claude.Calls()), f.writes())
		}
		if record := f.record(); record["phase"] != "preflight" || !strings.Contains(record["error"].(string), "preparing the Workspace") {
			t.Errorf("record = %v", record)
		}
	})

	// Ported from lib/fire.test.sh: test_state_dir_from_host_env_and_default.
	t.Run("the State dir comes from the Host config and defaults outside the checkout", func(t *testing.T) {
		f := newFireFixture(t, labelsConfig)
		f.queue = []githubtest.Reply{queueReply()}
		delete(f.environ, "AUTO_AGENT_STATE_DIR")
		if code := f.fire(); code != 0 {
			t.Fatalf("exit %d, stderr %q", code, f.stderr.String())
		}
		records, _ := filepath.Glob(filepath.Join(f.environ["HOME"], ".local", "state", "auto-agent", "acme", "widgets", "fires", "*.json"))
		if len(records) != 1 {
			t.Errorf("%d records under the default State dir", len(records))
		}
	})

	t.Run("without --repo the Target Project is the origin of the target dir", func(t *testing.T) {
		f := newFireFixture(t, labelsConfig)
		f.answer(finished)
		code := f.fire(f.dir)
		if record := f.record(); code != 0 || record["repo"] != "acme/widgets" || record["pr"] != 55.0 {
			t.Errorf("exit %d, record = %v", code, record)
		}
	})

	t.Run("the Workspace's remote is the Target Project over HTTPS unless the environment names another", func(t *testing.T) {
		repo := config.Repo{Owner: "acme", Name: "widgets"}
		none := func(string) (string, bool) { return "", false }
		other := func(string) (string, bool) { return "/srv/widgets.git", true }
		if got := gitRemote(repo, none); got != "https://github.com/acme/widgets.git" {
			t.Errorf("default remote = %q", got)
		}
		if got := gitRemote(repo, other); got != "/srv/widgets.git" {
			t.Errorf("overridden remote = %q", got)
		}
	})
}

// gateOf is the Gate verdict in the record's first Step entry.
func gateOf(t *testing.T, record map[string]any) map[string]any {
	t.Helper()
	steps := record["steps"].([]any)
	if len(steps) == 0 {
		t.Fatalf("the record has no Step entry: %v", record)
	}
	gate, _ := steps[0].(map[string]any)["gate"].(map[string]any)
	if gate == nil {
		t.Fatalf("the Step entry has no Gate verdict: %v", steps[0])
	}
	return gate
}

// loginHost turns the fixture into a login Host whose usage endpoint
// answers with body.
func (f *fireFixture) loginHost(body string) {
	f.t.Helper()
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, body) }))
	f.t.Cleanup(endpoint.Close)
	f.authStatus = `{"loggedIn":true,"authMethod":"claude.ai"}`
	delete(f.environ, "CLAUDE_CODE_OAUTH_TOKEN")
	f.environ[EnvUsageAPI] = endpoint.URL
	credentials := filepath.Join(f.environ["HOME"], ".claude", ".credentials.json")
	os.MkdirAll(filepath.Dir(credentials), 0o755)
	os.WriteFile(credentials, []byte(`{"claudeAiOauth":{"accessToken":"tok"}}`), 0o600)
}

func TestFireBudgetGate(t *testing.T) {
	const reset = "2026-10-12T14:00:00+00:00"
	usage := func(session, fable int) string {
		return fmt.Sprintf(`{"five_hour":{"utilization":%d,"resets_at":%q},"seven_day":{"utilization":10,"resets_at":"2026-10-16T00:00:00+00:00"},"seven_day_fable":{"utilization":%d,"resets_at":"2026-10-16T00:00:00+00:00"}}`, session, reset, fable)
	}

	t.Run("a Gate verdict is produced before the Step and written into its entry in the Fire record", func(t *testing.T) {
		f := newFireFixture(t, labelsConfig)
		f.loginHost(usage(19, 5))
		f.answer(finished)
		if code := f.fire(); code != 0 {
			t.Fatalf("exit %d, stderr %q", code, f.stderr.String())
		}
		record := f.record()
		gate := gateOf(t, record)
		if gate["authMode"] != "login" || gate["sensor"] != "usage-endpoint" || gate["state"] != "ok" || gate["remainPct"] != 81.0 || gate["shouldFire"] != true || record["resetAt"] != "" {
			t.Errorf("gate = %v", gate)
		}
		// The verdict was there while the Step ran.
		during := readJSON(t, filepath.Join(f.out, "in-flight.json"))
		if during["phase"] != "implement" {
			t.Errorf("in flight: %v", during)
		}
	})

	t.Run("a wait verdict ends the Fire before the Step runs: no label is applied and the reset is recorded", func(t *testing.T) {
		f := newFireFixture(t, labelsConfig)
		f.loginHost(usage(80, 5))
		if code := f.fire(); code != 0 {
			t.Fatalf("exit %d, want 0: waiting is not a failure (stderr %q)", code, f.stderr.String())
		}
		if f.writes() != "" || len(f.claude.Calls()) != 0 {
			t.Errorf("GitHub received %q; %d Step calls", f.writes(), len(f.claude.Calls()))
		}
		record := f.record()
		gate := gateOf(t, record)
		entry := record["steps"].([]any)[0].(map[string]any)
		if record["phase"] != "gate" || record["resetAt"] != reset || record["issue"] != 20.0 || record["error"] != "" ||
			gate["shouldFire"] != false || gate["remainPct"] != 20.0 || entry["name"] != "implement" || entry["outcome"] != "" {
			t.Errorf("record = %v", record)
		}
	})

	t.Run("the Model policy chooses the fallback model for the Step when the primary's own limit is spent", func(t *testing.T) {
		f := newFireFixture(t, labelsConfig)
		f.loginHost(usage(19, 97))
		f.environ["AUTO_AGENT_MODEL_PRIMARY"], f.environ["AUTO_AGENT_MODEL_FALLBACK"] = "fable", "sonnet"
		f.answer(finished)
		if code := f.fire(); code != 0 {
			t.Fatalf("exit %d, stderr %q", code, f.stderr.String())
		}
		if model, _ := f.claude.Calls()[0].Arg("--model"); model != "sonnet" {
			t.Errorf("the Step ran on %q, want the fallback", model)
		}
		if gate := gateOf(t, f.record()); gate["fireModel"] != "sonnet" || gate["shouldFire"] != true {
			t.Errorf("gate = %v", gate)
		}
	})

	t.Run("below the switch percent the Step runs on the primary", func(t *testing.T) {
		f := newFireFixture(t, labelsConfig)
		f.loginHost(usage(19, 94))
		f.environ["AUTO_AGENT_MODEL_PRIMARY"], f.environ["AUTO_AGENT_MODEL_FALLBACK"] = "fable", "sonnet"
		f.answer(finished)
		f.fire()
		if model, _ := f.claude.Calls()[0].Arg("--model"); model != "fable" {
			t.Errorf("the Step ran on %q, want the primary", model)
		}
	})

	t.Run("an EXHAUSTED Step feeds the next verdict: the next Fire waits for the reset", func(t *testing.T) {
		f := newFireFixture(t, labelsConfig)
		resetsAt := fireClock.Add(2 * time.Hour).Unix()
		f.claude.Script(steptest.Reply{ExitCode: 1, Lines: []string{
			steptest.Init("s", "claude-opus", append([]string{"StructuredOutput"}, implement.Spec().Tools...), []string{"factory:implement"}),
			steptest.RateLimit("rejected", resetsAt, "five_hour"),
		}})
		if code := f.fire(); code != 1 {
			t.Fatalf("first Fire: exit %d, want 1", code)
		}
		writes := f.writes()
		f.stdout.Reset()
		if code := f.fire(); code != 0 {
			t.Fatalf("second Fire: exit %d, want 0 (stderr %q)", code, f.stderr.String())
		}
		if len(f.claude.Calls()) != 1 || f.writes() != writes {
			t.Errorf("the second Fire ran a Step or wrote to GitHub: %d calls\n%s", len(f.claude.Calls()), f.writes())
		}
		var second map[string]any
		json.Unmarshal(f.stdout.Bytes(), &second)
		gate := gateOf(t, second)
		if gate["sensor"] != "stream-events" || gate["shouldFire"] != false || second["resetAt"] != "2026-10-12T11:30:00Z" {
			t.Errorf("second Fire = %v", second)
		}
	})

	t.Run("a dead Claude credential refuses the Step: no label, and the record says why", func(t *testing.T) {
		f := newFireFixture(t, labelsConfig)
		f.authErr = errors.New("exit status 1")
		if code := f.fire(); code != 1 {
			t.Fatalf("exit %d, want 1", code)
		}
		if f.writes() != "" || len(f.claude.Calls()) != 0 {
			t.Errorf("GitHub received %q; %d Step calls", f.writes(), len(f.claude.Calls()))
		}
		record := f.record()
		if gate := gateOf(t, record); gate["state"] != "auth-dead" || !strings.Contains(record["error"].(string), "Claude's credential is dead") {
			t.Errorf("record = %v", record)
		}
	})

	t.Run("an auth mode the Host config declares must agree with claude: a mismatch refuses the Step", func(t *testing.T) {
		f := newFireFixture(t, labelsConfig)
		f.environ["CLAUDE_AUTH_MODE"] = "login"
		if code := f.fire(); code != 1 || f.writes() != "" || len(f.claude.Calls()) != 0 {
			t.Fatalf("exit %d, writes %q, %d Step calls", code, f.writes(), len(f.claude.Calls()))
		}
		if record := f.record(); !strings.Contains(record["error"].(string), "auth mode mismatch") {
			t.Errorf("error = %v", record["error"])
		}
	})

	t.Run("a Fire with nothing to do never asks the Budget gate", func(t *testing.T) {
		f := newFireFixture(t, labelsConfig)
		f.queue = []githubtest.Reply{queueReply()}
		f.authErr = errors.New("claude must not be asked")
		if code := f.fire(); code != 0 {
			t.Fatalf("exit %d, stderr %q", code, f.stderr.String())
		}
	})
}
