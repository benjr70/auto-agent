package daemon

import (
	"context"
	"encoding/json"
	"os/exec"
	"regexp"
	"strings"
	"testing"

	"github.com/benjr70/auto-agent/internal/github"
	"github.com/benjr70/auto-agent/internal/github/githubtest"
	"github.com/benjr70/auto-agent/internal/state"
)

// The cases of lib/labels-ensure.test.sh, under their names.

func TestLabelTable(t *testing.T) {
	t.Run("exit 0, 20 rows of name/color/description, AFK and wayfinder:research among them", func(t *testing.T) {
		names := map[string]string{}
		for _, l := range state.HarnessLabels {
			names[l.Name] = l.Color
		}
		if len(state.HarnessLabels) != 20 || len(names) != 20 || names["AFK"] != "1D76DB" || names["wayfinder:research"] == "" {
			t.Errorf("table = %+v", state.HarnessLabels)
		}
	})
	t.Run("every row has a name, a 6-hex colour and a description", func(t *testing.T) {
		row := regexp.MustCompile(`^[A-Za-z:-]+\t[0-9A-F]{6}\t.+$`)
		for _, l := range state.HarnessLabels {
			if line := l.Name + "\t" + l.Color + "\t" + l.Description; !row.MatchString(line) {
				t.Errorf("malformed row %q", line)
			}
		}
	})
	t.Run("the table is the bash table, row for row", func(t *testing.T) {
		out, err := exec.Command("bash", "../../lib/labels-ensure.sh", "--list").Output()
		if err != nil {
			t.Fatalf("lib/labels-ensure.sh --list: %v", err)
		}
		var rows []string
		for _, l := range state.HarnessLabels {
			rows = append(rows, l.Name+"\t"+l.Color+"\t"+l.Description)
		}
		if got, want := strings.Join(rows, "\n"), strings.TrimSpace(string(out)); got != want {
			t.Errorf("Go table =\n%s\nbash table =\n%s", got, want)
		}
	})
}

// labelServer is a fake GitHub with the given labels in acme/widgets, that
// answers a create with status.
func labelServer(t *testing.T, status int, present ...string) (*githubtest.Server, *github.Client) {
	s := githubtest.New(t)
	list := []map[string]string{}
	for _, name := range present {
		list = append(list, map[string]string{"name": name})
	}
	s.Handle("GET", "/repos/acme/widgets/labels", githubtest.JSON(200, list))
	s.Handle("POST", "/repos/acme/widgets/labels", githubtest.JSON(status, map[string]string{}))
	return s, github.New(github.Options{BaseURL: s.URL, Token: "t"})
}

func created(t *testing.T, s *githubtest.Server) map[string]github.Label {
	got := map[string]github.Label{}
	for _, w := range s.Writes() {
		var l github.Label
		if w.Method != "POST" || w.Path != "/repos/acme/widgets/labels" || json.Unmarshal([]byte(w.Body), &l) != nil {
			t.Fatalf("unexpected write %+v", w)
		}
		got[l.Name] = l
	}
	return got
}

func TestEnsureLabels(t *testing.T) {
	ctx := context.Background()
	t.Run("only absent labels are created, each with an explicit colour and never --force", func(t *testing.T) {
		s, client := labelServer(t, 201, "AFK", "HITL", "bug", "wayfinder:map")
		made, present, err := EnsureLabels(ctx, client, "acme", "widgets")
		t.Run("exit 0 and the summary counts 17 created, 3 present", func(t *testing.T) {
			if err != nil || len(made) != 17 || present != 3 {
				t.Errorf("EnsureLabels = %d created, %d present, %v", len(made), present, err)
			}
		})
		got := created(t, s)
		t.Run("AFK, HITL and wayfinder:map were not re-created", func(t *testing.T) {
			for _, name := range []string{"AFK", "HITL", "wayfinder:map"} {
				if _, ok := got[name]; ok {
					t.Errorf("%s was re-created", name)
				}
			}
		})
		t.Run("spec and AFK:in-progress were created against the config's repo with a colour", func(t *testing.T) {
			if got["spec"].Color != "0052CC" || got["AFK:in-progress"].Color != "FBCA04" || got["spec"].Description == "" {
				t.Errorf("created = %+v", got)
			}
		})
		t.Run("one 'created <name>' line per created label", func(t *testing.T) {
			if len(got) != 17 || len(s.Writes()) != 17 {
				t.Errorf("%d labels created in %d writes", len(got), len(s.Writes()))
			}
		})
	})
	t.Run("everything already present is a no-op", func(t *testing.T) {
		var names []string
		for _, l := range state.HarnessLabels {
			names = append(names, l.Name)
		}
		s, client := labelServer(t, 201, names...)
		made, present, err := EnsureLabels(ctx, client, "acme", "widgets")
		if err != nil || len(made) != 0 || present != 20 || len(s.Writes()) != 0 {
			t.Errorf("EnsureLabels = %v, %d present, %v; writes %d", made, present, err, len(s.Writes()))
		}
	})
	t.Run("unreadable label list exits 1", func(t *testing.T) {
		s := githubtest.New(t)
		s.Handle("GET", "/repos/acme/widgets/labels", githubtest.Text(500, "boom"))
		if _, _, err := EnsureLabels(ctx, github.New(github.Options{BaseURL: s.URL, Token: "t"}), "acme", "widgets"); err == nil || len(s.Writes()) != 0 {
			t.Errorf("EnsureLabels = %v, writes %d", err, len(s.Writes()))
		}
	})
	t.Run("a label present in another case is present: GitHub's names are case-insensitive", func(t *testing.T) {
		var names []string
		for _, l := range state.HarnessLabels {
			names = append(names, strings.ToUpper(l.Name))
		}
		s, client := labelServer(t, 201, names...)
		made, present, err := EnsureLabels(ctx, client, "acme", "widgets")
		if err != nil || len(made) != 0 || present != 20 || len(s.Writes()) != 0 {
			t.Errorf("EnsureLabels = %v, %d present, %v; writes %d", made, present, err, len(s.Writes()))
		}
	})
	t.Run("a create GitHub refuses as already_exists is a label present, not a failure", func(t *testing.T) {
		s := githubtest.New(t)
		s.Handle("GET", "/repos/acme/widgets/labels", githubtest.JSON(200, []map[string]string{}))
		s.Handle("POST", "/repos/acme/widgets/labels", githubtest.JSON(422, map[string]any{
			"message": "Validation Failed",
			"errors":  []map[string]string{{"resource": "Label", "code": "already_exists", "field": "name"}},
		}))
		made, present, err := EnsureLabels(ctx, github.New(github.Options{BaseURL: s.URL, Token: "t"}), "acme", "widgets")
		if err != nil || len(made) != 0 || present != 20 || len(s.Writes()) != 20 {
			t.Errorf("EnsureLabels = %v, %d present, %v; writes %d", made, present, err, len(s.Writes()))
		}
	})
	t.Run("a failed create exits 1 after trying every label", func(t *testing.T) {
		s, client := labelServer(t, 422, "AFK", "HITL", "wayfinder:map")
		made, present, err := EnsureLabels(ctx, client, "acme", "widgets")
		if err == nil || len(made) != 0 || present != 3 || len(s.Writes()) != 17 || !strings.Contains(err.Error(), "spec") {
			t.Errorf("EnsureLabels = %v, %d present, %v; writes %d", made, present, err, len(s.Writes()))
		}
	})
}
