package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/benjr70/auto-agent/internal/github"
	"github.com/benjr70/auto-agent/internal/github/githubtest"
	"github.com/benjr70/auto-agent/internal/state"
)

const (
	depsConfig = `{"commit_scopes":["a","deps"],"commands":{"install":"x","test":"y"},"pick":{"labels":{}},"dependabot":{},"rounds":{"deps_fix":2}}`
	botBranch  = "dependabot/npm_and_yarn/axios-1.1.2"
	botPath    = "/repos/acme/widgets/pulls/60"
	// botChecklist is the Target Project's bot-pr-checklist.md.
	botChecklist = "## Manual verification\n\n- [ ] The tests pass after the bump\n"
	botBody      = "Bumps axios from 1.1.0 to 1.1.2."
)

// botWorld is Bot PR #60 as the fake GitHub keeps it.
type botWorld struct {
	f        *fireFixture
	author   string
	title    string
	body     string
	draft    bool
	labels   []string
	comments []ticketComment
	// mergeable is what the PR read alone says: true, or false for a
	// conflict.
	mergeable bool
	// commits are the PR's commits as GitHub lists them, by author login.
	commits []string
	// failComments makes GitHub refuse to list its comments, once okReads
	// reads of them have been answered.
	failComments bool
	okReads      int
	commentReads int
	// headRepo is where its branch lives.
	headRepo string
	// noSHA lists it with no head sha.
	noSHA bool
}

// newBotFixture is a Fire on a Target Project whose Harness config is
// harness, with a Bot-PR checklist on its default branch and Dependabot's
// branch on the remote, one bump commit ahead.
func newBotFixture(t *testing.T, harness string) (*fireFixture, *botWorld) {
	t.Helper()
	f := newFireFixture(t, harness)
	// No ticket waits: a Fire that takes no PR does nothing.
	f.queue = []githubtest.Reply{queueReply()}
	os.WriteFile(filepath.Join(f.dir, ".auto-agent", "bot-pr-checklist.md"), []byte(botChecklist), 0o644)
	git(t, f.dir, "add", "-A")
	git(t, f.dir, "commit", "--quiet", "-m", "the Bot-PR checklist")
	git(t, f.dir, "push", "--quiet", f.remote, "HEAD:trunk")
	git(t, f.dir, "checkout", "--quiet", "-b", botBranch)
	os.WriteFile(filepath.Join(f.dir, "package-lock.json"), []byte("axios 1.1.2\n"), 0o644)
	git(t, f.dir, "add", "-A")
	git(t, f.dir, "-c", "user.name=dependabot[bot]", "-c", "user.email=49699333+dependabot[bot]@users.noreply.github.com", "commit", "--quiet", "-m", "chore(deps): bump axios from 1.1.0 to 1.1.2")
	git(t, f.dir, "push", "--quiet", f.remote, "HEAD:refs/heads/"+botBranch)
	git(t, f.dir, "checkout", "--quiet", "trunk")
	w := &botWorld{f: f, headRepo: "acme/widgets", author: state.DependabotLogin, title: "chore(deps): bump axios from 1.1.0 to 1.1.2", body: botBody, mergeable: true, commits: []string{state.DependabotLogin}}
	w.serve()
	return f, w
}

// head is the Bot PR's head, as the remote has it.
func (w *botWorld) head() string { return w.f.remoteRef("refs/heads/" + botBranch) }

func (w *botWorld) json(mergeable bool) map[string]any {
	labels := []map[string]string{}
	for _, l := range w.labels {
		labels = append(labels, map[string]string{"name": l})
	}
	pr := map[string]any{
		"number": 60, "node_id": "PR_60", "title": w.title, "body": w.body, "state": "open", "draft": w.draft, "created_at": "2026-10-01T09:00:00Z",
		"user": map[string]any{"login": w.author}, "labels": labels,
		"head": map[string]any{"ref": botBranch, "sha": w.head(), "repo": map[string]any{"full_name": w.headRepo}},
		"base": map[string]any{"ref": "trunk"},
	}
	if mergeable {
		pr["mergeable"] = w.mergeable
	}
	return pr
}

func (w *botWorld) serve() {
	gh := w.f.gh
	gh.HandleFunc("GET", pullsPath, func(githubtest.Request) githubtest.Reply {
		listed := w.json(false)
		if w.noSHA {
			listed["head"].(map[string]any)["sha"] = ""
		}
		return githubtest.JSON(200, []map[string]any{listed})
	})
	gh.HandleFunc("GET", botPath, func(githubtest.Request) githubtest.Reply { return githubtest.JSON(200, w.json(true)) })
	gh.HandleFunc("PATCH", botPath, func(r githubtest.Request) githubtest.Reply {
		var sent map[string]string
		json.Unmarshal([]byte(r.Body), &sent)
		if title, ok := sent["title"]; ok {
			w.title = title
		}
		if body, ok := sent["body"]; ok {
			w.body = body
		}
		return githubtest.JSON(200, map[string]any{"number": 60})
	})
	gh.HandleFunc("GET", botPath+"/commits", func(githubtest.Request) githubtest.Reply {
		list := []map[string]any{}
		for _, login := range w.commits {
			list = append(list, map[string]any{"sha": "c", "author": map[string]any{"login": login}, "commit": map[string]any{"author": map[string]any{"name": login, "email": login + "@example.com"}}})
		}
		return githubtest.JSON(200, list)
	})
	gh.HandleFunc("GET", "/repos/acme/widgets/issues/60/comments", func(githubtest.Request) githubtest.Reply {
		w.commentReads++
		if w.failComments && w.commentReads > w.okReads {
			return githubtest.Text(502, "bad gateway")
		}
		return githubtest.JSON(200, w.comments)
	})
	gh.HandleFunc("POST", "/repos/acme/widgets/issues/60/comments", func(r githubtest.Request) githubtest.Reply {
		var sent struct{ Body string }
		json.Unmarshal([]byte(r.Body), &sent)
		w.comment("machine-user", sent.Body)
		return githubtest.JSON(201, map[string]any{"id": len(w.comments)})
	})
	gh.HandleFunc("POST", "/repos/acme/widgets/issues/60/labels", func(r githubtest.Request) githubtest.Reply {
		var sent struct{ Labels []string }
		json.Unmarshal([]byte(r.Body), &sent)
		w.labels = append(w.labels, sent.Labels...)
		return githubtest.Text(200, `[]`)
	})
}

func (w *botWorld) comment(author, body string) {
	w.comments = append(w.comments, ticketComment{ID: 6000 + len(w.comments), Body: body, User: map[string]string{"login": author}})
}

// posted are the bodies of the comments the Daemon posted on the Bot PR.
func (w *botWorld) posted() []string {
	var bodies []string
	for _, c := range w.comments {
		if c.User["login"] == "machine-user" {
			bodies = append(bodies, c.Body)
		}
	}
	return bodies
}

func depsOf(record map[string]any) map[string]any {
	deps, _ := record["deps"].(map[string]any)
	return deps
}

// reads are the GET requests GitHub was sent for path.
func (f *fireFixture) reads(path string) int {
	n := 0
	for _, r := range f.gh.Requests() {
		if r.Method == "GET" && r.Path == path {
			n++
		}
	}
	return n
}

// The Bot PR Lane, first half: a Dependabot PR taken in the reconcile Lane
// and proved by Tier A. With the triage cases the Fire runs
// (lib/pr-triage.test.sh) and the park and the cap
// (lib/deps-lane.test.sh), under their names.
func TestFireBotPR(t *testing.T) {
	t.Run("a patch bump with green CI is retitled, has its checklist and gets Tier A for its sha (behaviour 1)", func(t *testing.T) {
		f, w := newBotFixture(t, depsConfig)
		head := w.head()
		f.mustFire(0)
		record := f.record()
		deps := depsOf(record)
		if record["reconcile"] != "dependabot" || record["action"] != "deps-land" || record["issue"] != nil || record["pr"] != 60.0 || record["phase"] != "done" {
			t.Errorf("record %v", record)
		}
		if deps["outcome"] != "tier-a-green" || deps["tierA"] != "green" || deps["security"] != true || deps["retitled"] != true || deps["checklist"] != true || deps["head"] != head || record["notWorked"] != "Tier B, the gate and the merge are not in this build yet" {
			t.Errorf("deps %v, notWorked %v", deps, record["notWorked"])
		}
		if w.title != "fix(deps): bump axios from 1.1.0 to 1.1.2" || !strings.HasPrefix(w.body, botBody+"\n\n"+state.ChecklistMarker) || !strings.Contains(w.body, "- [ ] The tests pass after the bump") {
			t.Errorf("title %q, body %q", w.title, w.body)
		}
		if posted := w.posted(); len(posted) != 1 || !strings.Contains(posted[0], "<!-- deps-lane tierA=green sha="+head+" -->") {
			t.Errorf("comments %q", posted)
		}
		// No ticket: no lock, nothing on any issue but the PR's own.
		if writes := f.writes(); strings.Contains(writes, lockPath) || strings.Contains(writes, "/issues/20") || w.head() != head {
			t.Errorf("GitHub received\n%s", writes)
		}
		// The next Fire finds the marker for the head and leaves the PR
		// alone, writing nothing: what is left is not in this build, and
		// taking it again would hold the reconcile Lane on it forever.
		before := len(f.gh.Writes())
		f.mustFire(0)
		if record := f.record(); len(f.gh.Writes()) != before || record["reconcile"] != "" || record["deps"] != nil || !strings.Contains(fmt.Sprint(record["warnings"]), state.TierAWaiting(60)) {
			t.Errorf("record %v, GitHub received\n%s", record, f.writes())
		}
	})
	t.Run("dependabot.yml is looked up in the Target Project: a version bump keeps its chore title", func(t *testing.T) {
		f, w := newBotFixture(t, depsConfig)
		git(t, f.dir, "checkout", "--quiet", "trunk")
		os.MkdirAll(filepath.Join(f.dir, ".github"), 0o755)
		os.WriteFile(filepath.Join(f.dir, ".github", "dependabot.yml"), []byte("version: 2\n"), 0o644)
		git(t, f.dir, "add", "-A")
		git(t, f.dir, "commit", "--quiet", "-m", "dependabot version updates")
		git(t, f.dir, "push", "--quiet", f.remote, "HEAD:trunk")
		f.mustFire(0)
		if deps := depsOf(f.record()); deps["security"] != false || deps["retitled"] != false || w.title != "chore(deps): bump axios from 1.1.0 to 1.1.2" {
			t.Errorf("deps %v, title %q", deps, w.title)
		}
	})
	t.Run("park drafts the PR, labels it AFK:deps-failed and comments once: red CI twice with rounds.deps_fix 2, two Fix commits with the trailer, then parked (behaviour 2)", func(t *testing.T) {
		f, w := newBotFixture(t, depsConfig)
		f.redCI()
		f.fixer.Script(fixed("echo one > package-lock.json", "fix(deps): regenerate the lockfile"), fixed("echo two > package-lock.json", "fix(deps): regenerate it again"))
		f.mustFire(0)
		record := f.record()
		if deps := depsOf(record); len(f.fixer.Calls()) != 2 || deps["outcome"] != "parked" || deps["tierA"] != "failed" || deps["attempts"] != 2.0 || record["parkedPR"] != "AFK:deps-failed" || record["phase"] != "deps-failed" || record["error"] != "" {
			t.Errorf("%d Fixes, record %v", len(f.fixer.Calls()), record)
		}
		log := git(t, f.dir, "--git-dir", f.remote, "log", "--format=%B%x00", "trunk.."+botBranch)
		if commits := strings.Split(strings.TrimSuffix(strings.TrimSpace(log), "\x00"), "\x00"); len(commits) != 3 ||
			strings.TrimSpace(commits[0]) != "fix(deps): regenerate it again [dependabot skip]" || strings.TrimSpace(commits[1]) != "fix(deps): regenerate the lockfile [dependabot skip]" {
			t.Errorf("branch log %q", log)
		}
		// The draft, the label and one comment carrying the park's marker.
		if !strings.Contains(f.writes(), "POST /graphql") || strings.Join(w.labels, ",") != "AFK:deps-failed" {
			t.Errorf("labels %v, GitHub received\n%s", w.labels, f.writes())
		}
		posted := w.posted()
		head := w.head()
		if len(posted) != 3 || strings.Count(posted[1], "sha="+head+" -->") != 2 || !strings.Contains(posted[2], state.ParkMarker(head)) || !strings.Contains(posted[2], "Last failure: CI still failing: unit") {
			t.Errorf("comments %q", posted)
		}
		// The budget follows the head: two attempts are counted on it.
		if h, _ := state.ReadDepsHistory(commentsOf(w.comments), "machine-user", head); h.Attempts != 2 {
			t.Errorf("history %+v", h)
		}
	})
	t.Run("a Fix the CI answers is Tier A fixed(1), its marker on the pushed head", func(t *testing.T) {
		f, w := newBotFixture(t, depsConfig)
		f.redCI(green)
		f.fixer.Script(fixed("echo fixed > package-lock.json", "fix(deps): regenerate the lockfile"))
		f.mustFire(0)
		head := w.head()
		if deps := depsOf(f.record()); deps["tierA"] != "fixed(1)" || deps["outcome"] != "tier-a-green" || deps["attempts"] != 1.0 {
			t.Errorf("deps %v", deps)
		}
		if posted := w.posted(); len(posted) != 2 || !strings.Contains(posted[0], state.FixAttemptMarker(1, head)) || !strings.Contains(posted[1], "tierA=green sha="+head) {
			t.Errorf("comments %q", posted)
		}
		// The Fix was told the bump, with no ticket and no credential.
		call := f.fixer.Calls()[0]
		if !strings.Contains(call.Stdin, `"bump":{"pr":60`) || !strings.Contains(call.Stdin, `"issue":{"number":0`) || call.Env["GH_TOKEN"] != "" {
			t.Errorf("the Fix was given %s, env GH_TOKEN %q", call.Stdin, call.Env["GH_TOKEN"])
		}
	})
	t.Run("the fix cap is the Harness config's rounds.deps_fix", func(t *testing.T) {
		f, w := newBotFixture(t, strings.Replace(depsConfig, `"deps_fix":2`, `"deps_fix":1`, 1))
		f.redCI()
		f.fixer.Script(fixed("echo one > package-lock.json", "fix(deps): regenerate the lockfile"))
		f.mustFire(0)
		if deps := depsOf(f.record()); len(f.fixer.Calls()) != 1 || deps["cap"] != 1.0 || deps["outcome"] != "parked" || !strings.Contains(w.posted()[1], "fix budget of 1 fix attempts") {
			t.Errorf("%d Fixes, deps %v, comments %q", len(f.fixer.Calls()), deps, w.posted())
		}
	})
	t.Run("a bump with its budget spent has its CI read first: the last Fix is credited when it worked", func(t *testing.T) {
		f, w := newBotFixture(t, depsConfig)
		comment, _ := state.FixAttemptsComment(1, 2, 2, w.head())
		w.comment("machine-user", comment)
		f.mustFire(0)
		if deps := depsOf(f.record()); f.ciReads == 0 || len(f.fixer.Calls()) != 0 || deps["outcome"] != "tier-a-green" || deps["attempts"] != 2.0 || !strings.Contains(w.posted()[1], "tierA=green sha="+w.head()) {
			t.Errorf("%d CI reads, deps %v", f.ciReads, deps)
		}
	})
	t.Run("a bump with its budget spent and CI still red is parked with no Fix", func(t *testing.T) {
		f, w := newBotFixture(t, depsConfig)
		f.redCI()
		comment, _ := state.FixAttemptsComment(1, 2, 2, w.head())
		w.comment("machine-user", comment)
		f.mustFire(0)
		if deps := depsOf(f.record()); f.ciReads == 0 || len(f.fixer.Calls()) != 0 || deps["outcome"] != "parked" || !strings.Contains(w.posted()[1], "Last failure: CI still failing: unit") {
			t.Errorf("%d CI reads, deps %v, comments %q", f.ciReads, deps, w.posted())
		}
	})
	t.Run("park on a PR a human made ready again makes only the moves still owed", func(t *testing.T) {
		// A human made the PR ready again, the label off, on the same head:
		// it is parked again, and the comment it has already is not posted
		// twice.
		f, w := newBotFixture(t, depsConfig)
		f.redCI()
		comment, _ := state.FixAttemptsComment(1, 2, 2, w.head())
		w.comment("machine-user", comment)
		w.comment("machine-user", state.DepsParkedComment(2, "earlier", w.head()))
		f.mustFire(0)
		if writes := f.writes(); !strings.HasSuffix(writes, "\nPOST /graphql\nPOST /repos/acme/widgets/issues/60/labels") || len(w.posted()) != 2 {
			t.Errorf("GitHub received\n%s", writes)
		}
	})
	t.Run("park on an already-parked PR mutates nothing", func(t *testing.T) {
		// Parked by another hand between the Lane's read and its park: the
		// draft, the label and the comment are all there already.
		f, w := newBotFixture(t, depsConfig)
		f.redCI()
		comment, _ := state.FixAttemptsComment(1, 2, 2, w.head())
		w.comment("machine-user", comment)
		w.comment("machine-user", state.DepsParkedComment(2, "earlier", w.head()))
		w.title = "fix(deps): bump axios from 1.1.0 to 1.1.2"
		w.body, _ = state.InjectChecklist(botBody, botChecklist)
		reads := 0
		f.gh.HandleFunc("GET", botPath, func(githubtest.Request) githubtest.Reply {
			// The triage's read and the Lane's: then the park's.
			if reads++; reads > 2 {
				w.draft, w.labels = true, []string{state.LabelDepsFailed}
			}
			return githubtest.JSON(200, w.json(true))
		})
		f.mustFire(0)
		if deps := depsOf(f.record()); deps["outcome"] != "parked" || reads != 3 || len(f.gh.Writes()) != 0 || len(w.posted()) != 2 {
			t.Errorf("deps %v, %d PR reads, GitHub received\n%s", deps, reads, f.writes())
		}
	})
	t.Run("a marker for another sha counts for nothing", func(t *testing.T) {
		f, w := newBotFixture(t, depsConfig)
		const old = "0123456789abcdef0123456789abcdef01234567"
		tierA, _ := state.TierAComment(old)
		spent, _ := state.FixAttemptsComment(1, 2, 2, old)
		w.comment("machine-user", tierA)
		w.comment("machine-user", spent)
		f.mustFire(0)
		if deps := depsOf(f.record()); f.ciReads == 0 || deps["attempts"] != 0.0 || deps["outcome"] != "tier-a-green" || !strings.Contains(w.posted()[2], "sha="+w.head()) {
			t.Errorf("%d CI reads, deps %v", f.ciReads, deps)
		}
	})
	t.Run("a failed comment read never refunds the fix budget: it stops the lane", func(t *testing.T) {
		// The triage read them; the Lane's own read fails.
		f, w := newBotFixture(t, depsConfig)
		w.failComments, w.okReads = true, 1
		f.mustFire(1)
		if record := f.record(); !strings.Contains(record["error"].(string), "its fix attempts cannot be counted") || f.ciReads != 0 || len(f.fixer.Calls()) != 0 || len(f.gh.Writes()) != 0 {
			t.Errorf("record %v, GitHub received\n%s", record, f.writes())
		}
	})
	t.Run("inject reads the checklist path from the Harness config: with no bot-pr-checklist.md beside it the lane says so and stops", func(t *testing.T) {
		f, _ := newBotFixture(t, depsConfig)
		git(t, f.dir, "rm", "--quiet", ".auto-agent/bot-pr-checklist.md")
		git(t, f.dir, "commit", "--quiet", "-m", "no checklist")
		git(t, f.dir, "push", "--quiet", f.remote, "HEAD:trunk")
		f.mustFire(1)
		if record := f.record(); !strings.Contains(record["error"].(string), "no bot-pr-checklist.md") || f.ciReads != 0 {
			t.Errorf("record %v", record)
		}
	})
	t.Run("a conflict with only Dependabot's commits: one @dependabot rebase comment, nothing pushed (behaviour 3)", func(t *testing.T) {
		f, w := newBotFixture(t, depsConfig)
		w.mergeable = false
		head := w.head()
		f.mustFire(0)
		record := f.record()
		if record["reconcile"] != "conflict" || depsOf(record)["outcome"] != "nudged" || w.head() != head || f.ciReads != 0 {
			t.Errorf("record %v", record)
		}
		if writes := f.writes(); writes != "POST /repos/acme/widgets/issues/60/comments" || strings.Join(w.posted(), "|") != "@dependabot rebase" {
			t.Errorf("comments %q, GitHub received\n%s", w.posted(), writes)
		}
	})
	t.Run("a conflict on a branch the Daemon pushed to is rebased and pushed with a lease", func(t *testing.T) {
		f, w := newBotFixture(t, depsConfig)
		w.mergeable, w.commits = false, []string{state.DependabotLogin, "machine-user"}
		git(t, f.dir, "checkout", "--quiet", "trunk")
		os.WriteFile(filepath.Join(f.dir, "other.txt"), []byte("moved on\n"), 0o644)
		git(t, f.dir, "add", "-A")
		git(t, f.dir, "commit", "--quiet", "-m", "trunk moves")
		git(t, f.dir, "push", "--quiet", f.remote, "HEAD:trunk")
		head := w.head()
		f.mustFire(0)
		record := f.record()
		rebased := w.head()
		if depsOf(record)["outcome"] != "rebased" || rebased == head || record["commit"] != rebased || len(w.posted()) != 0 || f.ciReads != 0 {
			t.Errorf("record %v", record)
		}
		if base := git(t, f.dir, "--git-dir", f.remote, "merge-base", "trunk", botBranch); base != f.remoteRef("refs/heads/trunk") {
			t.Errorf("the branch is not on trunk: merge base %s", base)
		}
	})
	t.Run("a conflict a human must resolve is parked", func(t *testing.T) {
		f, w := newBotFixture(t, depsConfig)
		w.mergeable, w.commits = false, []string{state.DependabotLogin, "machine-user"}
		git(t, f.dir, "checkout", "--quiet", "trunk")
		os.WriteFile(filepath.Join(f.dir, "package-lock.json"), []byte("axios 1.0.9\n"), 0o644)
		git(t, f.dir, "add", "-A")
		git(t, f.dir, "commit", "--quiet", "-m", "trunk touches the lockfile")
		git(t, f.dir, "push", "--quiet", f.remote, "HEAD:trunk")
		head := w.head()
		f.mustFire(0)
		record := f.record()
		// It spent no fix attempt, and its comment does not say it did.
		if depsOf(record)["outcome"] != "parked" || w.head() != head || len(f.fixer.Calls()) != 0 || !strings.Contains(w.posted()[0], "no commands.lockfile_refresh (package-lock.json)") || strings.Contains(w.posted()[0], "fix budget") {
			t.Errorf("record %v, comments %q", record, w.posted())
		}
	})
	t.Run("a lockfile conflict goes to the Fix with the lockfile command", func(t *testing.T) {
		f, w := newBotFixture(t, strings.Replace(depsConfig, `"test":"y"`, `"test":"y","lockfile_refresh":"npm install --package-lock-only"`, 1))
		w.mergeable, w.commits = false, []string{state.DependabotLogin, "machine-user"}
		git(t, f.dir, "checkout", "--quiet", "trunk")
		os.WriteFile(filepath.Join(f.dir, "package-lock.json"), []byte("axios 1.0.9\n"), 0o644)
		git(t, f.dir, "add", "-A")
		git(t, f.dir, "commit", "--quiet", "-m", "trunk touches the lockfile")
		git(t, f.dir, "push", "--quiet", f.remote, "HEAD:trunk")
		f.fixer.Script(fixed("echo 'axios 1.1.2' > package-lock.json", "fix(deps): resolve the lockfile"))
		f.mustFire(0)
		if deps := depsOf(f.record()); deps["outcome"] != "rebased" || len(f.fixer.Calls()) != 1 || !strings.Contains(f.fixer.Calls()[0].Stdin, `"lockfile_refresh":"npm install --package-lock-only"`) {
			t.Errorf("deps %v", deps)
		}
	})
	t.Run("Dependabot impostors and parked shapes are refused", func(t *testing.T) {
		for name, change := range map[string]func(*botWorld){
			// behaviour 4: a dependabot/ branch opened by a human.
			"a human's dependabot/ branch":  func(w *botWorld) { w.author = "some-human" },
			"a fork's dependabot/ branch":   func(w *botWorld) { w.headRepo = "someone/widgets" },
			"a draft":                       func(w *botWorld) { w.draft = true },
			"AFK:deps-failed":               func(w *botWorld) { w.labels = []string{"AFK:deps-failed"} },
			"HITL with no approving review": func(w *botWorld) { w.labels = []string{"HITL"} },
		} {
			t.Run(name, func(t *testing.T) {
				f, w := newBotFixture(t, depsConfig)
				change(w)
				f.gh.Handle("GET", botPath+"/reviews", githubtest.JSON(200, []map[string]any{{"state": "CHANGES_REQUESTED", "user": map[string]string{"login": "the-human"}}}))
				f.mustFire(0)
				if record := f.record(); record["reconcile"] != "" || record["deps"] != nil || f.reads(botPath+"/commits") != 0 || len(f.gh.Writes()) != 0 {
					t.Errorf("record %v, GitHub received\n%s", record, f.writes())
				}
			})
		}
	})
	t.Run("HITL bot PR picked only once APPROVED", func(t *testing.T) {
		f, w := newBotFixture(t, depsConfig)
		w.labels = []string{"HITL"}
		f.gh.Handle("GET", botPath+"/reviews", githubtest.JSON(200, []map[string]any{
			{"state": "CHANGES_REQUESTED", "user": map[string]string{"login": "the-human"}},
			{"state": "APPROVED", "user": map[string]string{"login": "the-human"}},
		}))
		f.mustFire(0)
		if record := f.record(); record["reconcile"] != "dependabot" {
			t.Errorf("record %v", record)
		}
	})
	t.Run("bot verdicts are unworkable exactly when the deps-land lane is off: no Bot PR is taken, and the Fire record notes why", func(t *testing.T) {
		for config, why := range map[string]string{
			strings.Replace(depsConfig, `"dependabot":{},`, "", 1):                              "the Harness config declares no dependabot block",
			strings.Replace(depsConfig, `"dependabot":{}`, `"dependabot":{"enabled":false}`, 1): "dependabot.enabled is false",
		} {
			f, _ := newBotFixture(t, config)
			f.mustFire(0)
			record := f.record()
			want := fmt.Sprintf("Bot PR #60 is not taken: the Bot PR Lane is off (%s)", why)
			if warnings := fmt.Sprint(record["warnings"]); record["reconcile"] != "" || !strings.Contains(warnings, want) || f.reads(botPath+"/commits") != 0 || len(f.gh.Writes()) != 0 {
				t.Errorf("record %v", record)
			}
		}
	})
	t.Run("scan picks a Dependabot PR with its head sha", func(t *testing.T) {
		f, w := newBotFixture(t, depsConfig)
		f.mustFire(0)
		if record := f.record(); record["reconcile"] != "dependabot" || record["pr"] != 60.0 || depsOf(record)["head"] != w.head() {
			t.Errorf("record %v", record)
		}
	})
	t.Run("a listing without headRefOid costs no bot round trip", func(t *testing.T) {
		f, w := newBotFixture(t, depsConfig)
		w.noSHA = true
		f.mustFire(0)
		if record := f.record(); record["reconcile"] != "" || f.reads(botPath) != 0 || f.reads(botPath+"/commits") != 0 || w.commentReads != 0 || len(f.gh.Writes()) != 0 {
			t.Errorf("record %v", record)
		}
	})
	t.Run("a broken Dependabot probe yields no dependabot pick", func(t *testing.T) {
		f, _ := newBotFixture(t, depsConfig)
		f.gh.Handle("GET", botPath, githubtest.Text(502, "bad gateway"))
		f.mustFire(0)
		if record := f.record(); record["reconcile"] != "" || len(f.gh.Writes()) != 0 || !strings.Contains(fmt.Sprint(record["warnings"]), "whether PR #60 merges could not be read") {
			t.Errorf("record %v", record)
		}
	})
	t.Run("an Agent PR is worked before a Bot PR, and the Bot PR is not read", func(t *testing.T) {
		f, w := newBotFixture(t, depsConfig)
		agent := openPR(55, "feat/issue-20", "2026-10-12T09:00:00Z", "AFK:revise")
		f.gh.HandleFunc("GET", pullsPath, func(githubtest.Request) githubtest.Reply {
			return githubtest.JSON(200, []map[string]any{w.json(false), agent})
		})
		derived := triageDryRun(t, f)
		if !strings.Contains(derived, `"pr": 55`) || f.reads(botPath+"/commits") != 0 {
			t.Errorf("state %s", derived)
		}
	})
}

// commentsOf are the fake's comments as the client reads them.
func commentsOf(list []ticketComment) []github.Comment {
	comments := make([]github.Comment, len(list))
	for i, c := range list {
		comments[i] = github.Comment{ID: int64(c.ID), Author: c.User["login"], Body: c.Body}
	}
	return comments
}

// triageDryRun is what run --dry-run derives, as JSON.
func triageDryRun(t *testing.T, f *fireFixture) string {
	t.Helper()
	f.stdout.Reset()
	if code := f.run("run", "--once", "--dry-run", "--repo", "acme/widgets", f.dir); code != 0 {
		t.Fatalf("exit %d, stderr %s", code, f.stderr.String())
	}
	return f.stdout.String()
}
