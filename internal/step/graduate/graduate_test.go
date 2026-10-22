package graduate

import (
	"encoding/json"
	"errors"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/benjr70/auto-agent/internal/state"
)

func TestSpec(t *testing.T) {
	t.Run("the result schema names exactly the fields of the result and proposal types, all required", func(t *testing.T) {
		var s struct {
			Required   []string `json:"required"`
			Properties struct {
				Proposals struct {
					MaxItems *int `json:"maxItems"`
					Items    struct {
						Required             []string                   `json:"required"`
						Properties           map[string]json.RawMessage `json:"properties"`
						AdditionalProperties *bool                      `json:"additionalProperties"`
					} `json:"items"`
				} `json:"proposals"`
			} `json:"properties"`
		}
		if err := json.Unmarshal([]byte(schema), &s); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(s.Required, []string{"proposals"}) {
			t.Errorf("required = %v", s.Required)
		}
		items := s.Properties.Proposals.Items
		var want, got []string
		for i := range reflect.TypeFor[Proposal]().NumField() {
			want = append(want, reflect.TypeFor[Proposal]().Field(i).Tag.Get("json"))
		}
		for name := range items.Properties {
			got = append(got, name)
		}
		sort.Strings(want)
		sort.Strings(got)
		sort.Strings(items.Required)
		if !reflect.DeepEqual(got, want) || !reflect.DeepEqual(items.Required, want) || items.AdditionalProperties == nil || *items.AdditionalProperties {
			t.Errorf("properties %v, required %v, want %v and no others allowed", got, items.Required, want)
		}
		// Go refuses a result with too many, and the refusal is what the
		// correcting resume is told: a schema bound would only retry blind.
		if s.Properties.Proposals.MaxItems != nil {
			t.Error("the schema bounds the proposals itself")
		}
	})
	t.Run("the Step changes nothing: read tools and read-only git", func(t *testing.T) {
		spec := Spec()
		for _, tool := range spec.Tools {
			if tool == "Write" || tool == "Edit" {
				t.Errorf("the Step can edit: %v", spec.Tools)
			}
		}
		for _, rule := range spec.Allowed {
			if strings.HasPrefix(rule, "Bash(") && !strings.HasPrefix(rule, "Bash(git ") {
				t.Errorf("the Step may run %s", rule)
			}
		}
	})
	t.Run("the prompt carries the graduation rules and no GitHub or git-write instruction", func(t *testing.T) {
		for _, rule := range []string{"specifiable", "at most `max`", "`clears`", "`humanFree`", "`blockedByProposals`", "proposals: []", "Never answer a question you propose"} {
			if !strings.Contains(prompt, rule) {
				t.Errorf("the prompt lacks %q", rule)
			}
		}
		command := regexp.MustCompile("(?i)\\bgh \\w|\\bgit (add|commit|push|checkout|reset|rebase|stash|switch|tag|merge)\\b|--add-label|AFK|HITL")
		if found := command.FindAllString(prompt, -1); found != nil {
			t.Errorf("the prompt names %q", found)
		}
	})
}

func proposal(title string) Proposal {
	return Proposal{Title: title, Question: "What is " + title + "?", Type: "research", BlockedBy: []int{}, BlockedByProposals: []int{}}
}

func TestCheck(t *testing.T) {
	in := Input{Children: []Child{{Number: 30, Title: "Open one", Type: "task"}}, Max: Max}
	t.Run("no proposal is a valid result", func(t *testing.T) {
		if err := (Result{Proposals: []Proposal{}}).Check(in); err != nil {
			t.Error(err)
		}
	})
	t.Run("two proposals, one blocked by the other and by an open child, are valid", func(t *testing.T) {
		second := proposal("b")
		second.BlockedBy, second.BlockedByProposals = []int{30}, []int{1}
		if err := (Result{Proposals: []Proposal{proposal("a"), second}}).Check(in); err != nil {
			t.Error(err)
		}
	})
	// Behaviour 3.
	t.Run("over 3 proposals are refused, saying the bound", func(t *testing.T) {
		r := Result{Proposals: []Proposal{proposal("a"), proposal("b"), proposal("c"), proposal("d")}}
		if err := r.Check(in); err == nil || !strings.Contains(err.Error(), "proposes 4 tickets; at most 3") {
			t.Errorf("Check = %v", err)
		}
	})
	for name, edit := range map[string]func(*Proposal){
		"a title on two lines":                         func(p *Proposal) { p.Title = "a\nb" },
		"an empty question":                            func(p *Proposal) { p.Question = " " },
		"a type that is not wayfinder's":               func(p *Proposal) { p.Type = "spike" },
		"a blocker that is not an open child":          func(p *Proposal) { p.BlockedBy = []int{31} },
		"a blocking proposal that does not come first": func(p *Proposal) { p.BlockedByProposals = []int{2} },
		"a proposal blocked by itself":                 func(p *Proposal) { p.BlockedByProposals = []int{1} },
	} {
		t.Run(name+" is refused", func(t *testing.T) {
			p := proposal("a")
			edit(&p)
			if err := (Result{Proposals: []Proposal{p, proposal("b")}}).Check(in); err == nil || !strings.HasPrefix(err.Error(), "proposal 1") {
				t.Errorf("Check = %v", err)
			}
		})
	}
}

func TestDepth(t *testing.T) {
	bodies := map[int]string{
		1: "## Question\n\nhuman-made\n",
		2: "## Question\n\nq\n\nSpawned by #1\n",
		3: "## Question\n\nq\n\nSpawned by #2\n",
		4: "Spawned by #5\n",
		5: "Spawned by #4\n",
	}
	read := func(n int) (string, error) {
		if body, ok := bodies[n]; ok {
			return body, nil
		}
		return "", errors.New("not found")
	}
	for _, c := range []struct {
		name  string
		body  string
		depth int
	}{
		{"a ticket a human created is depth 0", bodies[1], 0},
		{"one it spawned is depth 1", bodies[2], 1},
		{"one that one spawned is depth 2", bodies[3], 2},
		{"deeper is read no further than it takes to tell", "Spawned by #3\n", MaxAFKDepth},
		{"prose naming a ticket mid-line is no provenance", "This was Spawned by #3 in prose", 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got, err := Depth(c.body, read); err != nil || got != c.depth {
				t.Errorf("Depth = %d, %v; want %d", got, err, c.depth)
			}
		})
	}
	t.Run("a chain that cannot be read is an error", func(t *testing.T) {
		if _, err := Depth("Spawned by #99\n", read); err == nil {
			t.Error("no error")
		}
	})
	t.Run("a chain that loops is an error", func(t *testing.T) {
		if _, err := Depth("Spawned by #4\n", func(n int) (string, error) {
			if n == 4 {
				return "Spawned by #4\n", nil
			}
			return read(n)
		}); err == nil {
			t.Error("no error")
		}
	})
}

func TestRoute(t *testing.T) {
	for _, c := range []struct {
		typ       string
		humanFree bool
		depth     int
		want      string
	}{
		{"research", false, 1, state.LabelAFK},
		{"research", true, 2, state.LabelAFK},
		{"task", true, 2, state.LabelAFK},
		{"task", false, 1, state.LabelHITL},
		// Behaviour 2: depth 3 is HITL whatever the Step says.
		{"research", true, 3, state.LabelHITL},
		{"task", true, 3, state.LabelHITL},
		// A grilling or a prototype is a human's, human-free or not.
		{"grilling", true, 1, state.LabelHITL},
		{"prototype", true, 1, state.LabelHITL},
	} {
		if got := Route(c.typ, c.humanFree, c.depth); got != c.want {
			t.Errorf("Route(%s, human-free %v, depth %d) = %s, want %s", c.typ, c.humanFree, c.depth, got, c.want)
		}
	}
}

func TestBody(t *testing.T) {
	p := proposal("x")
	p.Question = "\nWhich one?\n"
	if got := Body(p, 20); got != "## Question\n\nWhich one?\n\nSpawned by #20\n" {
		t.Errorf("Body = %q", got)
	}
	if n, ok := SpawnedBy(Body(p, 20)); !ok || n != 20 {
		t.Errorf("SpawnedBy(Body) = %d, %v", n, ok)
	}
}

const fogMap = "## Destination\n\nA Daemon.\n\n## Decisions so far\n\n- [Old](u): old\n\n## Not yet specified\n\n- How the gate learns of a merge\n- Which board, and the webhooks it needs\n\n## Out of scope\n\n- Webhooks.\n"

func TestClearFog(t *testing.T) {
	t.Run("a whole bullet goes, line and all", func(t *testing.T) {
		got, ok := ClearFog(fogMap, "How the gate learns of a merge")
		if want := strings.Replace(fogMap, "- How the gate learns of a merge\n", "", 1); !ok || got != want {
			t.Errorf("ClearFog = %v\n%s", ok, got)
		}
	})
	t.Run("part of a line goes, and the rest of the line stays", func(t *testing.T) {
		got, ok := ClearFog(fogMap, ", and the webhooks it needs")
		if want := strings.Replace(fogMap, "- Which board, and the webhooks it needs\n", "- Which board\n", 1); !ok || got != want {
			t.Errorf("ClearFog = %v\n%s", ok, got)
		}
	})
	t.Run("text that is only outside Not yet specified is not cut, whatever section has it", func(t *testing.T) {
		for _, text := range []string{"Webhooks.", "A Daemon.", "old", "Not in the map at all"} {
			if got, ok := ClearFog(fogMap, text); ok || got != fogMap {
				t.Errorf("ClearFog(%q) = %v\n%s", text, ok, got)
			}
		}
	})
	t.Run("the Destination and Out of scope are byte-identical after a clear", func(t *testing.T) {
		got, _ := ClearFog(fogMap, "Which board, and the webhooks it needs")
		for _, part := range []string{"## Destination\n\nA Daemon.\n\n", "## Out of scope\n\n- Webhooks.\n"} {
			if !strings.Contains(got, part) {
				t.Errorf("%q changed:\n%s", part, got)
			}
		}
	})
	t.Run("a Map with no Not yet specified, or an empty text, is left as it is", func(t *testing.T) {
		if _, ok := ClearFog("## Destination\n\nx\n", "x"); ok {
			t.Error("cleared a Map without fog")
		}
		if _, ok := ClearFog(fogMap, "  "); ok {
			t.Error("cleared nothing")
		}
	})
}
