package cli

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/benjr70/auto-agent/internal/github/githubtest"
	"github.com/benjr70/auto-agent/internal/step/verifier"
	"github.com/benjr70/auto-agent/internal/verify/providertest"
)

// These tests run whole Fires with nothing in the queue and PR #61 merged
// from feat/issue-9, its one deferred item tagged for the Deployed tier. The
// deployed command is a fake that answers status; the Verify Step is the
// fixture's fake claude.
//
// Ported from lib/deployed-tier.test.sh, under its case names: the lane-off
// cases, "lane off (…): pick and status exit 3, no PR is listed, the command
// never runs", where Go has no exit code but lists no merged PR and runs no
// command; and of "the fixture with a deployed block ticks its deferred items
// from a status run (AC 1)", "status resolves the live environment's targets
// itself" and "both deferred items are ticked; the untagged item and the stray
// box are not", against a fake command in place of the fixture's live
// service: Go reads its block, and the Verify Step, not the test, exercises
// the items. Dropped from it: "the fixture service stands up as the live
// environment", which booted the fixture's service by hand to stand in for
// one; and the usage cases ("unknown subcommand: exit 2", "no Harness config:
// exit 2"), as Go has no deployed subcommand to misuse.

const (
	liveItem = "the greeting reaches prod <!-- post-deploy: GET $API_URL/greeting gives hello -->"
	// mergedPRPath is PR #61, merged.
	mergedPRPath = "/repos/acme/widgets/pulls/61"
	mergedRounds = "/repos/acme/widgets/issues/61/comments"
)

// deployedWorld is what the fake GitHub keeps of PR #61.
type deployedWorld struct {
	mergedAt string
	body     string
	comments []map[string]any
	// command is the deployed command.
	command *providertest.Fake
}

// newDeployedFixture is a Fire's world with an empty queue and PR #61
// merged mergedAt; verification is the Harness config's verification block,
// with the fake command's path for COMMAND.
func newDeployedFixture(t *testing.T, mergedAt, verification string) (*fireFixture, *deployedWorld) {
	t.Helper()
	w := &deployedWorld{mergedAt: mergedAt, body: checklistBody("the greeting says hello", liveItem)}
	w.command = providertest.New(t, providertest.Script{Status: []providertest.Reply{{Stdout: "API_URL=https://prod.example/api\n", Stderr: "probing prod\n"}}})
	cfg := `{"commit_scopes":["a"],"commands":{"install":"x","test":"y"},"pick":{"labels":{}},` +
		`"verification":` + strings.ReplaceAll(verification, "COMMAND", w.command.Path) + `,` +
		`"surfaces":{"api":{"kind":"api","url_key":"API_URL","paths":["api/**"]}}}`
	f := newFireFixture(t, cfg)
	f.queue = []githubtest.Reply{queueReply()}
	pr := openPR(61, "feat/issue-9", "2026-10-01T00:00:00Z")
	pr["title"], pr["state"], pr["merged_at"], pr["merge_commit_sha"] = "feat(a): the greeting", "closed", mergedAt, "m61"
	f.gh.HandleFunc("GET", pullsPath, func(r githubtest.Request) githubtest.Reply {
		if r.Query.Get("state") != "closed" {
			return githubtest.Text(200, `[]`)
		}
		listed := map[string]any{}
		for k, v := range pr {
			listed[k] = v
		}
		listed["body"] = w.body
		return githubtest.JSON(200, []map[string]any{listed})
	})
	f.gh.HandleFunc("GET", mergedPRPath, func(githubtest.Request) githubtest.Reply {
		one := map[string]any{}
		for k, v := range pr {
			one[k] = v
		}
		one["body"] = w.body
		return githubtest.JSON(200, one)
	})
	f.gh.HandleFunc("PATCH", mergedPRPath, func(r githubtest.Request) githubtest.Reply {
		var sent struct{ Body string }
		json.Unmarshal([]byte(r.Body), &sent)
		w.body = sent.Body
		return githubtest.JSON(200, map[string]any{"number": 61})
	})
	f.gh.HandleFunc("GET", mergedRounds, func(githubtest.Request) githubtest.Reply { return githubtest.JSON(200, w.comments) })
	f.gh.HandleFunc("POST", mergedRounds, func(r githubtest.Request) githubtest.Reply {
		var sent struct{ Body string }
		json.Unmarshal([]byte(r.Body), &sent)
		now := fireClock
		if !f.now.IsZero() {
			now = f.now
		}
		w.comments = append(w.comments, map[string]any{"id": 6100 + len(w.comments), "body": sent.Body, "user": map[string]any{"login": "machine-user"},
			"created_at": now.Format(time.RFC3339)})
		return githubtest.JSON(201, map[string]any{"id": 6100 + len(w.comments)})
	})
	f.gh.Handle("GET", "/repos/acme/widgets/issues/9", githubtest.JSON(200, map[string]any{"number": 9, "title": "Greet in prod", "body": "## What\n\nGreet.\n", "state": "closed"}))
	return f, w
}

// deployedOn is a Harness config whose deployed tier is on, its enabled
// omitted.
const deployedOn = `{"hermetic":{"command":"COMMAND","smoke":true},"deployed":{"command":"COMMAND"}}`

// rounds are the Deployed tier rounds on PR #61.
func (w *deployedWorld) rounds() []string {
	var bodies []string
	for _, c := range w.comments {
		bodies = append(bodies, c["body"].(string))
	}
	return bodies
}

func deployedOf(record map[string]any) map[string]any {
	d, _ := record["deployed"].(map[string]any)
	return d
}

func TestFireDeployed(t *testing.T) {
	t.Run("a PR merged an hour ago with one post-deploy item: picked, status called, item verified and ticked, round comment posted (behaviour 1)", func(t *testing.T) {
		f, w := newDeployedFixture(t, "2026-10-12T08:30:00Z", deployedOn)
		f.verifier.Script(verdicts(nil, verifier.Verdict{Text: liveItem, Verdict: "PASS", Class: verifier.DeployedEnv, Evidence: []string{"GET https://prod.example/api/greeting → 200 hello"}}))
		f.mustFire(0)
		if calls := strings.Join(w.command.Calls(), "|"); calls != "status" {
			t.Errorf("the deployed command was called %s, want status alone", calls)
		}
		if !strings.Contains(w.body, "- [x] "+liveItem+"\n") || !strings.Contains(w.body, "- [ ] the greeting says hello\n") || !strings.Contains(w.body, "- [ ] not a verification item") {
			t.Errorf("the body is\n%s", w.body)
		}
		if rounds := w.rounds(); len(rounds) != 1 || !strings.HasPrefix(rounds[0], "### Deployed verification — round 1/3\n\n**Result: 1/1 PASS, 0 deferred, 0 FAIL.**") ||
			!strings.Contains(rounds[0], "- GET https://prod.example/api/greeting → 200 hello") {
			t.Errorf("the rounds are %q", rounds)
		}
		// Go makes every write to GitHub: the tick and the comment, and
		// nothing on the ticket, which is closed and takes no lock.
		if want := "PATCH " + mergedPRPath + "\nPOST " + mergedRounds; f.writes() != want {
			t.Errorf("GitHub received\n%s\nwant\n%s", f.writes(), want)
		}
		record := f.record()
		d := deployedOf(record)
		if record["kind"] != "deployed" || record["action"] != "deployed" || record["phase"] != "done" || record["pr"] != 61.0 || record["issue"] != 9.0 ||
			d["line"] != "deployed-verify: 1/1 PASS, 0 deferred, 0 FAIL — round 1/3" || d["done"] != true || d["round"] != 1.0 || d["max"] != 3.0 || record["error"] != "" {
			t.Errorf("record = %v", record)
		}
		input := f.verifyInput(t)
		if !input.Deployed || len(input.Items) != 1 || input.Items[0].Text != liveItem || input.Items[0].PostDeploy != "GET $API_URL/greeting gives hello" ||
			strings.Join(input.Block, ",") != "API_URL=https://prod.example/api" || input.PR != 61 || input.Issue.Number != 9 || input.Issue.Title != "Greet in prod" ||
			len(input.Surfaces) != 1 || input.Surfaces[0].URL != "https://prod.example/api" || input.Surfaces[0].Tour {
			t.Errorf("the Step was told %+v", input)
		}
		if f.provider.Calls() != nil {
			t.Errorf("the hermetic provider was called %v", f.provider.Calls())
		}
	})

	t.Run("status resolves the live environment's targets itself", func(t *testing.T) {
		f, w := newDeployedFixture(t, "2026-10-12T08:30:00Z", deployedOn)
		w.command.Script(providertest.Script{Status: []providertest.Reply{{Stdout: "API_URL=https://live.example/api\nRELEASE=v7\n"}}})
		f.verifier.Script(verdicts(nil, verifier.Verdict{Text: liveItem, Verdict: "PASS", Class: verifier.DeployedEnv, Evidence: []string{"200"}}))
		f.mustFire(0)
		// The Harness config names no host: the block the command printed is
		// the round's one address book, from the default branch's checkout.
		input := f.verifyInput(t)
		if strings.Join(input.Block, ",") != "API_URL=https://live.example/api,RELEASE=v7" || input.Surfaces[0].URL != "https://live.example/api" {
			t.Errorf("the Step was told %+v", input)
		}
		if dir := w.command.Dir(); dir != filepath.Join(f.state, "clone") {
			t.Errorf("status ran in %s", dir)
		}
	})

	t.Run("both deferred items are ticked; the untagged item and the stray box are not", func(t *testing.T) {
		f, w := newDeployedFixture(t, "2026-10-12T08:30:00Z", deployedOn)
		const restart = "the item survives a restart <!-- post-deploy: GET $API_URL/items -->"
		w.body = "## Summary\n\n- [ ] a stray box <!-- post-deploy: not a verification item -->\n\n## Manual verification\n\n" +
			"- [x] already signed off <!-- post-deploy: GET /api/health -->\n- [ ] the page renders the list\n- [ ] " + liveItem + "\n\n" +
			"## Human verification required\n\n- [ ] " + restart + "\n"
		f.verifier.Script(verdicts(nil,
			verifier.Verdict{Text: liveItem, Verdict: "PASS", Class: verifier.DeployedEnv, Evidence: []string{"GET /greeting → 200"}},
			verifier.Verdict{Text: restart, Verdict: "PASS", Class: verifier.DeployedEnv, Evidence: []string{"GET /items → 200"}}))
		f.mustFire(0)
		for _, want := range []string{"- [x] " + liveItem + "\n", "- [x] " + restart + "\n", "- [ ] the page renders the list\n", "- [ ] a stray box", "- [x] already signed off"} {
			if !strings.Contains(w.body, want) {
				t.Errorf("the body lacks %q:\n%s", want, w.body)
			}
		}
		if input := f.verifyInput(t); len(input.Items) != 2 || input.Items[1].Section != "human" {
			t.Errorf("the Step was told %+v", input.Items)
		}
	})

	t.Run("the Verify Step is read-only: it sees no tool that edits", func(t *testing.T) {
		f, _ := newDeployedFixture(t, "2026-10-12T08:30:00Z", deployedOn)
		f.verifier.Script(verdicts(nil, verifier.Verdict{Text: liveItem, Verdict: "PASS", Class: verifier.DeployedEnv, Evidence: []string{"200"}}))
		f.mustFire(0)
		calls := f.verifier.Calls()
		if len(calls) != 1 {
			t.Fatalf("%d calls", len(calls))
		}
		if tools, _ := calls[0].Arg("--tools"); tools != "Read,Glob,Grep,Bash" {
			t.Errorf("the Step was given the tools %q", tools)
		}
	})

	t.Run("a PR merged 10 minutes ago: not picked yet (behaviour 2)", func(t *testing.T) {
		f, w := newDeployedFixture(t, "2026-10-12T09:20:00Z", deployedOn)
		f.mustFire(0)
		record := f.record()
		if record["kind"] != "idle" || record["action"] != "none" || record["deployed"] != nil || w.command.Calls() != nil || len(f.verifier.Calls()) != 0 || f.writes() != "" {
			t.Errorf("record = %v, calls %v, writes %q", record, w.command.Calls(), f.writes())
		}
		// Past the wait window, it is.
		f.later(25 * time.Minute)
		f.verifier.Script(verdicts(nil, verifier.Verdict{Text: liveItem, Verdict: "PASS", Class: verifier.DeployedEnv, Evidence: []string{"200"}}))
		f.mustFire(0)
		if f.record()["kind"] != "deployed" || len(w.rounds()) != 1 {
			t.Errorf("record = %v", f.record())
		}
	})

	t.Run("status exits 1: infra error recorded, no item verdict, the round still counted (behaviour 3)", func(t *testing.T) {
		f, w := newDeployedFixture(t, "2026-10-12T08:30:00Z", deployedOn)
		w.command.Script(providertest.Script{Status: []providertest.Reply{{Exit: 1, Stderr: "the tunnel is down\n"}, {Stdout: "API_URL=https://prod.example/api\n"}}})
		before := w.body
		f.mustFire(0)
		rounds := w.rounds()
		if len(rounds) != 1 || !strings.HasPrefix(rounds[0], "### Deployed verification — round 1/3: infrastructure error\n\n**No verdicts.**") ||
			!strings.Contains(rounds[0], "status exited 1 (the deployed environment is unhealthy): the tunnel is down") {
			t.Fatalf("the rounds are %q", rounds)
		}
		if strings.Contains(rounds[0], "Evidence directory:") {
			t.Errorf("the infra round's comment names an evidence directory:\n%s", rounds[0])
		}
		if left, _ := filepath.Glob(filepath.Join(f.state, "verify", "*", "*")); len(left) != 0 {
			t.Errorf("the infra round left %v", left)
		}
		if w.body != before || len(f.verifier.Calls()) != 0 || strings.Join(w.command.Calls(), "|") != "status" {
			t.Errorf("an item was acted on: body changed %v, Step calls %d, command calls %v", w.body != before, len(f.verifier.Calls()), w.command.Calls())
		}
		d := deployedOf(f.record())
		if d["infra"] != "status exited 1 (the deployed environment is unhealthy): the tunnel is down" ||
			d["line"] != "deployed-verify: infra-error — status exited 1 (the deployed environment is unhealthy): the tunnel is down (0 items verified)" || d["done"] != false {
			t.Errorf("deployed = %v", d)
		}
		// Quiet for the wait window again: the next round is round 2.
		f.later(31 * time.Minute)
		f.verifier.Script(verdicts(nil, verifier.Verdict{Text: liveItem, Verdict: "PASS", Class: verifier.DeployedEnv, Evidence: []string{"200"}}))
		f.mustFire(0)
		if rounds := w.rounds(); len(rounds) != 2 || !strings.HasPrefix(rounds[1], "### Deployed verification — round 2/3\n") {
			t.Errorf("the rounds are %q", rounds)
		}
	})

	t.Run("a missing url_key in the status block is an infra error, never an item verdict", func(t *testing.T) {
		f, w := newDeployedFixture(t, "2026-10-12T08:30:00Z", deployedOn)
		w.command.Script(providertest.Script{Status: []providertest.Reply{{Stdout: "WEB_URL=https://prod.example\n"}}})
		f.mustFire(0)
		if rounds := w.rounds(); len(rounds) != 1 || !strings.Contains(rounds[0], "surface api declares url_key API_URL, which the status block does not carry") || len(f.verifier.Calls()) != 0 {
			t.Errorf("the rounds are %q", rounds)
		}
	})

	t.Run("a failed item is reported in the comment, not fixed, and stays unticked", func(t *testing.T) {
		f, w := newDeployedFixture(t, "2026-10-12T08:30:00Z", deployedOn)
		f.verifier.Script(verdicts(nil, verifier.Verdict{Text: liveItem, Verdict: "FAIL", Class: verifier.DeployedEnv, Evidence: []string{"GET https://prod.example/api/greeting → 500"}}))
		f.mustFire(0)
		if !strings.Contains(w.body, "- [ ] "+liveItem+"\n") || len(f.fixer.Calls()) != 0 {
			t.Errorf("the body is\n%s", w.body)
		}
		if rounds := w.rounds(); len(rounds) != 1 || !strings.Contains(rounds[0], "— FAIL (deployed-env)\n\n- GET https://prod.example/api/greeting → 500") {
			t.Errorf("the rounds are %q", rounds)
		}
		if want := "POST " + mergedRounds; f.writes() != want {
			t.Errorf("GitHub received\n%s", f.writes())
		}
	})

	t.Run("the round cap: a PR with its rounds spent is not picked", func(t *testing.T) {
		f, w := newDeployedFixture(t, "2026-10-12T08:30:00Z", deployedOn)
		for i := 1; i <= 3; i++ {
			w.comments = append(w.comments, map[string]any{"id": i, "body": fmt.Sprintf("### Deployed verification — round %d/3\n", i), "user": map[string]any{"login": "machine-user"}, "created_at": "2026-10-01T00:00:00Z"})
		}
		f.mustFire(0)
		if f.record()["kind"] != "idle" || w.command.Calls() != nil {
			t.Errorf("record = %v", f.record())
		}
	})

	for _, c := range []struct{ name, verification string }{
		{"off", `{"hermetic":{"command":"COMMAND","smoke":true},"deployed":{"command":"COMMAND","enabled":false}}`},
		{"absent", `{"hermetic":{"command":"COMMAND","smoke":true}}`},
	} {
		t.Run("lane off ("+c.name+"): pick and status exit 3, no PR is listed, the command never runs", func(t *testing.T) {
			f, w := newDeployedFixture(t, "2026-10-12T08:30:00Z", c.verification)
			f.mustFire(0)
			for _, r := range f.gh.Requests() {
				if r.Path == pullsPath && r.Query.Get("state") == "closed" {
					t.Errorf("the merged PRs were listed: %v", r.Query)
				}
			}
			record := f.record()
			notes, _ := record["notes"].([]any)
			wantNotes := 0
			// Only a declared Lane switched off is noted.
			if c.name == "off" {
				wantNotes = 1
			}
			if record["kind"] != "idle" || w.command.Calls() != nil || len(notes) != wantNotes ||
				(wantNotes == 1 && notes[0] != "deployed-lane: off — verification.deployed.enabled is false") {
				t.Errorf("record = %v, calls %v", record, w.command.Calls())
			}
		})
	}
}
