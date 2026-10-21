package cli

import (
	"strings"
	"testing"

	"github.com/benjr70/auto-agent/internal/github/githubtest"
)

const pickProjectConfig = `{"commit_scopes":["a"],"commands":{"install":"x","test":"y"},"pick":{"project":{"number":7,"priority_field":"Priority","order":["P0","P1"]}}}`

// pickBoard scripts board 7 with a Priority field of P0 and P1 and issue
// 41, its add, and its Priority edit answered with edit.
func pickBoard(f *fixture, edit githubtest.Reply) {
	f.gh.HandleGraphQL("field(name: $field)", githubtest.Text(200, `{"data":{"repository":{"issue":{"id":"I_41"},"owner":{"projectV2":{"id":"PVT_7",
		"field":{"id":"F_prio","options":[{"id":"O_p0","name":"P0"},{"id":"O_p1","name":"P1"}]}}}}}}`))
	f.gh.HandleGraphQL("addProjectV2ItemById", githubtest.Text(200, `{"data":{"addProjectV2ItemById":{"item":{"id":"PVTI_41"}}}}`))
	f.gh.HandleGraphQL("updateProjectV2ItemFieldValue", edit)
	f.gh.Handle("POST", "/repos/acme/widgets/issues/41/labels", githubtest.Text(200, `[]`))
}

func TestPick(t *testing.T) {
	t.Run("the usage text lists pick", func(t *testing.T) {
		f := newFixture(t, labelsConfig)
		f.run()
		if !strings.Contains(f.stderr.String(), "pick add <issue> [--priority <name>]") || !strings.Contains(f.stderr.String(), "pick remove <issue>") {
			t.Errorf("usage = %s", f.stderr.String())
		}
	})
	t.Run("add under a label-only pick labels the ticket AFK and says so", func(t *testing.T) {
		f := newFixture(t, labelsConfig)
		f.gh.Handle("POST", "/repos/acme/widgets/issues/41/labels", githubtest.Text(200, `[]`))
		if code := f.run("pick", "add", "41", f.dir); code != 0 {
			t.Fatalf("exit %d: %s", code, f.stderr.String())
		}
		if got := f.stdout.String(); got != "pick: #41 is on the pick signal: AFK\n" {
			t.Errorf("stdout = %q", got)
		}
		if w := f.gh.Writes(); len(w) != 1 || w[0].Path != "/repos/acme/widgets/issues/41/labels" {
			t.Errorf("writes = %+v", w)
		}
	})
	t.Run("add under a Project pick puts the ticket on the board with the named Priority", func(t *testing.T) {
		f := newFixture(t, pickProjectConfig)
		pickBoard(f, githubtest.Text(200, `{"data":{"updateProjectV2ItemFieldValue":{"projectV2Item":{"id":"PVTI_41"}}}}`))
		if code := f.run("pick", "add", "41", "--priority", "P1", f.dir); code != 0 {
			t.Fatalf("exit %d: %s", code, f.stderr.String())
		}
		if got := f.stdout.String(); got != "pick: #41 is on the pick signal: AFK, on project 7 with Priority P1\n" {
			t.Errorf("stdout = %q", got)
		}
	})
	t.Run("an unknown priority name exits non-zero with the reason, before any write", func(t *testing.T) {
		f := newFixture(t, pickProjectConfig)
		code := f.run("pick", "add", "41", "--priority", "urgent", f.dir)
		if code != exitUsage || !strings.Contains(f.stderr.String(), "unknown-priority") || len(f.gh.Writes()) != 0 || f.stdout.Len() != 0 {
			t.Errorf("exit %d, stderr %q, writes %d", code, f.stderr.String(), len(f.gh.Writes()))
		}
	})
	t.Run("a Project write that fails exits non-zero with the reason and never says added", func(t *testing.T) {
		f := newFixture(t, pickProjectConfig)
		pickBoard(f, githubtest.Text(200, `{"errors":[{"message":"Resource not accessible by integration"}]}`))
		code := f.run("pick", "add", "41", f.dir)
		if code != exitFailed || !strings.Contains(f.stderr.String(), "priority-edit-failed") || !strings.Contains(f.stderr.String(), "Resource not accessible") || f.stdout.Len() != 0 {
			t.Errorf("exit %d, stdout %q, stderr %q", code, f.stdout.String(), f.stderr.String())
		}
	})
	t.Run("remove under a Project pick takes AFK off and the ticket off the board", func(t *testing.T) {
		f := newFixture(t, pickProjectConfig)
		f.gh.Handle("DELETE", "/repos/acme/widgets/issues/41/labels/AFK", githubtest.Text(200, `[]`))
		f.gh.HandleGraphQL("projectItems(", githubtest.Text(200, `{"data":{"repository":{"issue":{"projectItems":{"nodes":[{"id":"PVTI_41","project":{"id":"PVT_7"}}],"pageInfo":{"hasNextPage":false}}},"owner":{"projectV2":{"id":"PVT_7"}}}}}`))
		f.gh.HandleGraphQL("deleteProjectV2Item", githubtest.Text(200, `{"data":{"deleteProjectV2Item":{"deletedItemId":"PVTI_41"}}}`))
		if code := f.run("pick", "remove", "41", f.dir); code != 0 {
			t.Fatalf("exit %d: %s", code, f.stderr.String())
		}
		if got := f.stdout.String(); got != "pick: #41 is off the pick signal: no AFK, taken off project 7\n" {
			t.Errorf("stdout = %q", got)
		}
		if w := f.gh.Writes(); len(w) != 2 {
			t.Errorf("writes = %+v", w)
		}
	})
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"publish without --issue exits 2", []string{"pick", "add"}},
		{"unknown subcommand exits 2", []string{"pick", "frobnicate", "1"}},
		{"an issue that is not a number exits 2", []string{"pick", "add", "#41"}},
		{"remove takes no --priority", []string{"pick", "remove", "41", "--priority", "P0"}},
		{"labels takes no issue", []string{"pick", "labels", "41", "."}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, labelsConfig)
			if code := f.run(tc.args...); code != exitUsage || len(f.gh.Requests()) != 0 {
				t.Errorf("exit %d, requests %d: %s", code, len(f.gh.Requests()), f.stderr.String())
			}
		})
	}
	t.Run("labels ensures the harness labels and touches no ticket", func(t *testing.T) {
		f := newFixture(t, labelsConfig)
		f.gh.Handle("GET", "/repos/acme/widgets/labels", githubtest.JSON(200, []map[string]string{{"name": "AFK"}}))
		f.gh.Handle("POST", "/repos/acme/widgets/labels", githubtest.JSON(201, map[string]string{}))
		if code := f.run("pick", "labels", f.dir); code != 0 {
			t.Fatalf("exit %d: %s", code, f.stderr.String())
		}
		if !strings.HasSuffix(f.stdout.String(), "pick: labels created 19, present 1\n") || len(f.gh.Writes()) != 19 {
			t.Errorf("stdout %q, writes %d", f.stdout.String(), len(f.gh.Writes()))
		}
	})
	t.Run("no Harness config exits non-zero", func(t *testing.T) {
		f := newFixture(t, labelsConfig)
		if code := f.run("pick", "add", "41", t.TempDir()); code != exitFailed || len(f.gh.Writes()) != 0 {
			t.Errorf("exit %d: %s", code, f.stderr.String())
		}
	})
}
