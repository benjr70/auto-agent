package cli

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/benjr70/auto-agent/internal/fire"
	"github.com/benjr70/auto-agent/internal/github/githubtest"
)

// bootstrapConfig is a Target Project in the Bootstrap state: no hermetic
// tier, one Surface.
const bootstrapConfig = `{"commit_scopes":["a"],"commands":{"install":"x","test":"y"},"pick":{"labels":{}},"verification":{},
	"surfaces":{"api":{"kind":"api","url_key":"API_URL","paths":["api/**"]}}}`

// bootstrapIssue is an open AFK issue as the fake GitHub lists it.
type bootstrapIssue struct {
	Number int    `json:"number"`
	Title  string `json:"title"`
	Body   string `json:"body"`
}

// bootstrapGitHub makes the fake GitHub keep the open AFK issues, open new
// ones as #41 on, and answer the list with listErr when it is set. It
// returns the bodies of the issues opened, in order.
type bootstrapGitHub struct {
	open    []bootstrapIssue
	opened  []map[string]any
	listErr bool
}

func newBootstrapGitHub(gh *githubtest.Server, open ...bootstrapIssue) *bootstrapGitHub {
	b := &bootstrapGitHub{open: open}
	gh.HandleFunc("GET", "/repos/acme/widgets/issues", func(r githubtest.Request) githubtest.Reply {
		if r.Query.Get("labels") != "AFK" {
			return githubtest.Text(200, `[]`)
		}
		if b.listErr {
			return githubtest.Text(500, `{"message":"boom"}`)
		}
		return githubtest.JSON(200, b.open)
	})
	gh.HandleFunc("POST", "/repos/acme/widgets/issues", func(r githubtest.Request) githubtest.Reply {
		var sent map[string]any
		json.Unmarshal([]byte(r.Body), &sent)
		b.opened = append(b.opened, sent)
		issue := bootstrapIssue{Number: 40 + len(b.opened), Title: sent["title"].(string), Body: sent["body"].(string)}
		b.open = append(b.open, issue)
		return githubtest.JSON(201, issue)
	})
	gh.Handle("POST", "/repos/acme/widgets/issues/41/labels", githubtest.Text(200, `[]`))
	return b
}

// bootstrapFire is a Fire fixture over config with an empty queue, so a
// Fire does nothing but what the Bootstrap state owes.
func bootstrapFire(t *testing.T, config string, open ...bootstrapIssue) (*fireFixture, *bootstrapGitHub) {
	t.Helper()
	f := newFireFixture(t, config)
	f.queue = []githubtest.Reply{queueReply()}
	return f, newBootstrapGitHub(f.gh, open...)
}

func (f *fireFixture) creates() int {
	n := 0
	for _, w := range f.gh.Writes() {
		if w.Method == "POST" && w.Path == "/repos/acme/widgets/issues" {
			n++
		}
	}
	return n
}

// The `issue` cases of lib/bootstrap-state.test.sh, under their names.
func TestBootstrapIssue(t *testing.T) {
	marked := bootstrapIssue{Number: 41, Title: "Somebody renamed it", Body: fire.BootstrapMarker + "\n\nRewritten by a human."}
	for name, test := range map[string]func(t *testing.T){
		"the bootstrap ticket is opened once and reused on every re-run": func(t *testing.T) {
			f, b := bootstrapFire(t, bootstrapConfig)
			var got []any
			for range 3 {
				if code := f.fire(); code != 0 {
					t.Fatalf("exit %d: %s", code, f.stderr.String())
				}
				got = append(got, f.record()["bootstrap"])
			}
			if got[0] != "issue #41 created" || got[1] != "issue #41 reused" || got[2] != "issue #41 reused" || f.creates() != 1 {
				t.Fatalf("records say %v, %d issues opened", got, f.creates())
			}
			labels, _ := b.opened[0]["labels"].([]any)
			if len(labels) != 1 || labels[0] != "AFK" || b.opened[0]["title"] != fire.BootstrapTitle {
				t.Errorf("opened %v", b.opened[0])
			}
			// The pick helper put it on the pick signal.
			if !strings.Contains(f.writes(), "POST /repos/acme/widgets/issues/41/labels") {
				t.Errorf("not put on the pick signal; writes:\n%s", f.writes())
			}
		},
		"the ticket is found by its body marker, not its title": func(t *testing.T) {
			// The lowest of two wins; an unmarked one is not it.
			f, _ := bootstrapFire(t, bootstrapConfig,
				bootstrapIssue{Number: 39, Title: fire.BootstrapTitle, Body: "not the marker"},
				bootstrapIssue{Number: 45, Body: fire.BootstrapMarker}, marked)
			if code := f.fire(); code != 0 {
				t.Fatalf("exit %d: %s", code, f.stderr.String())
			}
			if got := f.record()["bootstrap"]; got != "issue #41 reused" || f.creates() != 0 {
				t.Errorf("bootstrap = %v, %d issues opened", got, f.creates())
			}
		},
		"the ticket names the contract and the reference providers": func(t *testing.T) {
			f, b := bootstrapFire(t, bootstrapConfig)
			f.fire()
			if len(b.opened) != 1 {
				t.Fatalf("%d issues opened", len(b.opened))
			}
			body := b.opened[0]["body"].(string)
			for _, want := range []string{fire.BootstrapMarker, "plugin/providers/CONTRACT.md", "plugin/providers/compose/provider", "provider-check",
				"## Acceptance criteria", "- `api` (api): its url_key is `API_URL`"} {
				if !strings.Contains(body, want) {
					t.Errorf("the body has no %q:\n%s", want, body)
				}
			}
		},
		"--dry-run reaches the same verdict and writes nothing": func(t *testing.T) {
			f := newFixture(t, bootstrapConfig)
			b := newBootstrapGitHub(f.gh)
			f.gh.HandleGraphQL("issues(first: 100", queueReply())
			if code := f.run("run", "--once", "--dry-run", f.dir); code != 0 {
				t.Fatalf("exit %d: %s", code, f.stderr.String())
			}
			if got := f.report(t)["bootstrap"]; got != "would open" {
				t.Errorf("bootstrap = %v", got)
			}
			b.open = []bootstrapIssue{marked}
			f.stdout.Reset()
			if code := f.run("run", "--once", "--dry-run", f.dir); code != 0 {
				t.Fatalf("exit %d: %s", code, f.stderr.String())
			}
			if got := f.report(t)["bootstrap"]; got != "would reuse #41" {
				t.Errorf("bootstrap = %v", got)
			}
			if writes := f.gh.Writes(); len(writes) != 0 {
				t.Errorf("the dry run made write requests: %+v", writes)
			}
		},
		"outside the Bootstrap state no ticket is owed and gh is never asked": func(t *testing.T) {
			// The Fire fixture declares a hermetic tier.
			f, _ := bootstrapFire(t, labelsConfig)
			if code := f.fire(); code != 0 {
				t.Fatalf("exit %d: %s", code, f.stderr.String())
			}
			if got := f.record()["bootstrap"]; got != "" || f.creates() != 0 {
				t.Errorf("bootstrap = %v, %d issues opened", got, f.creates())
			}
			f.stdout.Reset()
			if code := f.run("run", "--once", "--dry-run", f.dir); code != 0 {
				t.Fatalf("exit %d: %s", code, f.stderr.String())
			}
			if _, said := f.report(t)["bootstrap"]; said {
				t.Errorf("report = %s", f.stdout.String())
			}
			for _, r := range f.gh.Requests() {
				if r.Path == "/repos/acme/widgets/issues" && r.Query.Get("labels") == "AFK" {
					t.Error("the open AFK issues were listed")
				}
			}
		},
		"a gh failure is reported, never swallowed": func(t *testing.T) {
			f, _ := bootstrapFire(t, bootstrapConfig)
			f.gh.Handle("POST", "/repos/acme/widgets/issues", githubtest.Text(500, `{"message":"boom"}`))
			// The Fire goes on with its work: the next one opens it.
			if code := f.fire(); code != 0 {
				t.Fatalf("exit %d: %s", code, f.stderr.String())
			}
			record := f.record()
			if record["bootstrap"] != "" || !strings.Contains(warnings(record), "the Bootstrap issue could not be opened") {
				t.Errorf("record = %v", record)
			}
		},
		"an unreadable issue list never opens a SECOND bootstrap ticket": func(t *testing.T) {
			f, b := bootstrapFire(t, bootstrapConfig, marked)
			b.listErr = true
			if code := f.fire(); code != 0 {
				t.Fatalf("exit %d: %s", code, f.stderr.String())
			}
			record := f.record()
			if f.creates() != 0 || record["bootstrap"] != "" || !strings.Contains(warnings(record), "no second Bootstrap issue is opened") {
				t.Errorf("%d issues opened, record = %v", f.creates(), record)
			}
		},
	} {
		t.Run(name, test)
	}
}

func TestBootstrapIssueProjectPick(t *testing.T) {
	t.Run("under a Project pick the new ticket is put on the board, with the first Priority", func(t *testing.T) {
		f, _ := bootstrapFire(t, `{"commit_scopes":["a"],"commands":{"install":"x","test":"y"},"verification":{},
			"pick":{"project":{"number":7,"priority_field":"Priority","order":["P0","P1"]}}}`)
		edit := githubtest.Text(200, `{"data":{"updateProjectV2ItemFieldValue":{"projectV2Item":{"id":"PVTI_41"}}}}`)
		pickBoard(f.fixture, edit)
		if code := f.fire(); code != 0 {
			t.Fatalf("exit %d: %s", code, f.stderr.String())
		}
		if got := f.record()["bootstrap"]; got != "issue #41 created" {
			t.Errorf("bootstrap = %v (warnings %s)", got, warnings(f.record()))
		}
		added, set := false, false
		for _, w := range f.gh.Writes() {
			added = added || strings.Contains(w.Body, "addProjectV2ItemById")
			set = set || strings.Contains(w.Body, "updateProjectV2ItemFieldValue") && strings.Contains(w.Body, "O_p0")
		}
		if !added || !set {
			t.Errorf("on the board %v, Priority P0 set %v; writes:\n%s", added, set, f.writes())
		}
	})
}

// warnings are a Fire record's warnings, one per line.
func warnings(record map[string]any) string {
	var lines []string
	list, _ := record["warnings"].([]any)
	for _, w := range list {
		lines = append(lines, w.(string))
	}
	return strings.Join(lines, "\n")
}
