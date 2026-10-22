package cli

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/benjr70/auto-agent/internal/github/githubtest"
	"github.com/benjr70/auto-agent/internal/step/graduate"
	"github.com/benjr70/auto-agent/internal/step/research"
	"github.com/benjr70/auto-agent/internal/step/steptest"
)

// These tests run whole research Fires on ticket #20 of Map #9, and follow
// its resolve into the Graduate fog Step, whose fake claude is scripted.
//
// Ported: the graduation cases of lib/pick-publish.test.sh are
// internal/pick's TestAdd and TestRemove under their names, and those of
// lib/labels-ensure.test.sh internal/daemon's TestEnsureLabels: the Step
// reaches both through pick.Add and daemon.EnsureLabels. None is dropped.

// proposed is the fake Graduate fog Step proposing the given tickets.
func proposed(proposals ...graduate.Proposal) steptest.Reply {
	return steptest.Reply{Lines: []string{
		steptest.Init("graduate-1", "claude-opus", append([]string{"StructuredOutput"}, graduate.Spec().Tools...), []string{"factory:" + graduate.Name}),
		steptest.Success(graduate.Result{Proposals: append([]graduate.Proposal{}, proposals...)}),
	}}
}

// fogMapBody is Map #9 with fog in its Not yet specified.
const fogMapBody = "## Destination\n\nA Daemon any project can run.\n\n## Notes\n\nPrimary sources first.\n\n## Decisions so far\n\n- [Old](https://github.com/acme/widgets/issues/3): old\n\n" +
	"## Not yet specified\n\n- How a merged research PR is announced\n- Which board the Daemon watches\n\n## Out of scope\n\n- Webhooks.\n"

// created is an issue the Daemon opened while graduating fog.
type created struct {
	Title, Body string
	Labels      []string
}

// graduation is what the fake GitHub keeps of the tickets fog graduation
// creates: each is numbered from #40, with database id 4000 and up.
type graduation struct {
	created []created
	// subIssues are the database ids added under Map #9; blockedBy the
	// edges, as "#blocked<-id"; labelled the labels added to new tickets,
	// as "#n label".
	subIssues, blockedBy, labelled []string
	// refuseSubIssue makes GitHub refuse every sub-issue add.
	refuseSubIssue bool
}

// graduating sets up the research world for fog graduation: Map #9 with
// fog and one open child, #30, and GitHub keeping what is created.
func graduating(t *testing.T) (*fireFixture, *graduation) {
	t.Helper()
	f := newResearchFixture(t, mapParent)
	f.research.mapBody = fogMapBody
	f.research.children = []map[string]any{
		{"id": 3000, "number": 30, "title": "Open one", "state": "open", "labels": []map[string]any{{"name": "wayfinder:task"}}},
		{"id": 2000, "number": 20, "title": "Which merge gate?", "state": "closed"},
		{"id": 3100, "number": 31, "title": "Done one", "state": "closed"},
	}
	f.allLabels()
	g := &graduation{}
	f.gh.HandleFunc("POST", "/repos/acme/widgets/issues", func(r githubtest.Request) githubtest.Reply {
		var sent created
		json.Unmarshal([]byte(r.Body), &sent)
		g.created = append(g.created, sent)
		n := 39 + len(g.created)
		return githubtest.JSON(201, map[string]any{"id": 3960 + len(g.created)*40, "number": n, "title": sent.Title, "state": "open"})
	})
	f.gh.HandleFunc("POST", "/repos/acme/widgets/issues/9/sub_issues", func(r githubtest.Request) githubtest.Reply {
		if g.refuseSubIssue {
			return githubtest.Text(422, `{"message":"no"}`)
		}
		var sent struct {
			ID int64 `json:"sub_issue_id"`
		}
		json.Unmarshal([]byte(r.Body), &sent)
		g.subIssues = append(g.subIssues, strconv.FormatInt(sent.ID, 10))
		return githubtest.JSON(201, map[string]any{"number": 9})
	})
	blocked := regexp.MustCompile(`^/repos/acme/widgets/issues/([0-9]+)/dependencies/blocked_by$`)
	f.gh.HandleMatch("POST", blocked, func(r githubtest.Request) githubtest.Reply {
		var sent struct {
			ID int64 `json:"issue_id"`
		}
		json.Unmarshal([]byte(r.Body), &sent)
		g.blockedBy = append(g.blockedBy, fmt.Sprintf("#%s<-%d", blocked.FindStringSubmatch(r.Path)[1], sent.ID))
		return githubtest.JSON(201, map[string]any{})
	})
	labels := regexp.MustCompile(`^/repos/acme/widgets/issues/(4[0-9])/labels$`)
	f.gh.HandleMatch("POST", labels, func(r githubtest.Request) githubtest.Reply {
		var sent struct{ Labels []string }
		json.Unmarshal([]byte(r.Body), &sent)
		for _, label := range sent.Labels {
			g.labelled = append(g.labelled, "#"+labels.FindStringSubmatch(r.Path)[1]+" "+label)
		}
		return githubtest.Text(200, `[]`)
	})
	return f, g
}

func gproposal(title, typ, clears string) graduate.Proposal {
	return graduate.Proposal{Title: title, Question: title + "?", Type: typ, BlockedBy: []int{}, BlockedByProposals: []int{}, Clears: clears}
}

// graduatedOf is the resolve block's graduated tickets.
func graduatedOf(record map[string]any) []map[string]any {
	var out []map[string]any
	list, _ := resolveOf(record)["graduated"].([]any)
	for _, entry := range list {
		out = append(out, entry.(map[string]any))
	}
	return out
}

// graduationWrites are the writes after the ticket's own resolve, from its
// AFK:failed coming off to the lock released.
func graduationWrites(f *fireFixture) string {
	all := f.writes()
	from := strings.Index(all, "DELETE /repos/acme/widgets/issues/20/labels/AFK:failed\n")
	return strings.TrimSuffix(all[from+len("DELETE /repos/acme/widgets/issues/20/labels/AFK:failed\n"):], "\nDELETE "+unlockPath)
}

func TestFireGraduateFog(t *testing.T) {
	// Behaviour 1.
	t.Run("two proposals, one blocked by the other: both created, sub-issued, the edge wired, the Map edited, then both AFK, last", func(t *testing.T) {
		f, g := graduating(t)
		second := gproposal("Which board does the Daemon watch", "task", "Which board the Daemon watches")
		second.BlockedBy, second.BlockedByProposals, second.HumanFree = []int{30}, []int{1}, true
		f.graduator.Script(proposed(gproposal("How is a merged research PR announced", "research", "How a merged research PR is announced"), second))
		f.claude.Script(f.researched("research-1", findingsPath, findings("2026-10-12", true), "", answered))
		f.mustFire(0)

		want := strings.Join([]string{
			"POST /repos/acme/widgets/issues", "POST /repos/acme/widgets/issues/9/sub_issues",
			"POST /repos/acme/widgets/issues", "POST /repos/acme/widgets/issues/9/sub_issues",
			"POST /repos/acme/widgets/issues/41/dependencies/blocked_by", "POST /repos/acme/widgets/issues/41/dependencies/blocked_by",
			"PATCH /repos/acme/widgets/issues/9",
			"POST /repos/acme/widgets/issues/40/labels", "POST /repos/acme/widgets/issues/41/labels",
		}, "\n")
		if got := graduationWrites(f); got != want {
			t.Fatalf("after the resolve GitHub received\n%s\nwant\n%s", got, want)
		}
		if len(g.created) != 2 || g.created[0].Title != "How is a merged research PR announced" ||
			g.created[0].Body != "## Question\n\nHow is a merged research PR announced?\n\nSpawned by #20\n" || strings.Join(g.created[0].Labels, ",") != "wayfinder:research" ||
			strings.Join(g.created[1].Labels, ",") != "wayfinder:task" {
			t.Errorf("created %+v", g.created)
		}
		if strings.Join(g.subIssues, ",") != "4000,4040" || strings.Join(g.blockedBy, ",") != "#41<-3000,#41<-4000" {
			t.Errorf("sub-issues %v, edges %v", g.subIssues, g.blockedBy)
		}
		if strings.Join(g.labelled, ",") != "#40 AFK,#41 AFK" {
			t.Errorf("labelled %v", g.labelled)
		}
		t.Run("the Map gains the Decisions so far line and loses the fog, in one edit; its Destination and Out of scope are byte-identical", func(t *testing.T) {
			line := "- [Which merge gate?](https://github.com/acme/widgets/issues/20): A research PR is merged by the docs-only gate once CI is green."
			want := strings.Replace(fogMapBody, "issues/3): old\n", "issues/3): old\n"+line+"\n", 1)
			want = strings.Replace(want, "- How a merged research PR is announced\n- Which board the Daemon watches\n", "", 1)
			if f.research.mapBody != want {
				t.Errorf("the Map is\n%s\nwant\n%s", f.research.mapBody, want)
			}
			for _, heading := range []string{"Destination", "Out of scope"} {
				if research.Section(f.research.mapBody, heading) != research.Section(fogMapBody, heading) {
					t.Errorf("the Map's %s was edited", heading)
				}
			}
		})
		t.Run("the Step ran once, told the ticket, its answer, the Map and its open children", func(t *testing.T) {
			calls := f.graduator.Calls()
			if len(calls) != 1 {
				t.Fatalf("%d calls", len(calls))
			}
			var in graduate.Input
			json.Unmarshal([]byte(strings.TrimPrefix(calls[0].Stdin, "/factory:"+graduate.Name+" ")), &in)
			if in.Ticket.Number != 20 || in.Ticket.Question != "Which merge gate lands a research PR?" || in.Answer != answered.Gist || in.Findings != findingsPath ||
				in.Map.Number != 9 || in.Map.Destination != "A Daemon any project can run." || in.Map.OutOfScope != "- Webhooks." ||
				in.Map.NotYetSpecified != "- How a merged research PR is announced\n- Which board the Daemon watches" || !strings.Contains(in.Map.Decisions, "[Old]") ||
				len(in.Children) != 1 || in.Children[0] != (graduate.Child{Number: 30, Title: "Open one", Type: "task"}) || in.Max != 3 {
				t.Errorf("the Step was told %+v", in)
			}
		})
		t.Run("the record carries the new tickets; none was worked in this Fire", func(t *testing.T) {
			record := f.record()
			got := graduatedOf(record)
			if len(got) != 2 || got[0]["issue"] != 40.0 || got[0]["route"] != "AFK" || got[0]["depth"] != 1.0 || got[0]["wired"] != true || got[0]["picked"] != true ||
				got[1]["issue"] != 41.0 || got[1]["type"] != "task" || got[1]["picked"] != true || record["warnings"] != nil && len(record["warnings"].([]any)) != 0 {
				t.Errorf("graduated %v, warnings %v", got, record["warnings"])
			}
			if steps := stepsOf(record); len(steps) != 2 || steps[1]["name"] != graduate.Name || steps[1]["outcome"] != "OK" {
				t.Errorf("steps = %v", steps)
			}
			if len(f.claude.Calls()) != 1 || f.labelled() != "AFK:done" || f.research.ticketState != "closed" {
				t.Errorf("%d research calls; the ticket is %s, %q", len(f.claude.Calls()), f.research.ticketState, f.labelled())
			}
		})
	})

	// Behaviour 2.
	t.Run("a proposal at depth 3 is HITL, whatever the Step says, and never goes on the pick signal", func(t *testing.T) {
		f, g := graduating(t)
		f.research.ticketBody = "## Question\n\nWhich merge gate lands a research PR?\n\nSpawned by #18\n"
		f.gh.Handle("GET", "/repos/acme/widgets/issues/18", githubtest.JSON(200, map[string]any{"number": 18, "body": "## Question\n\nq\n\nSpawned by #17\n", "state": "closed"}))
		f.gh.Handle("GET", "/repos/acme/widgets/issues/17", githubtest.JSON(200, map[string]any{"number": 17, "body": "## Question\n\nhuman-made\n", "state": "closed"}))
		p := gproposal("Deeper still", "research", "")
		p.HumanFree = true
		f.graduator.Script(proposed(p))
		f.claude.Script(f.researched("research-1", findingsPath, findings("2026-10-12", true), "", answered))
		f.mustFire(0)
		if len(g.created) != 1 || strings.Join(g.created[0].Labels, ",") != "wayfinder:research,HITL" || len(g.labelled) != 0 {
			t.Errorf("created %+v, labelled %v", g.created, g.labelled)
		}
		if got := graduatedOf(f.record()); len(got) != 1 || got[0]["depth"] != 3.0 || got[0]["route"] != "HITL" || got[0]["picked"] != false {
			t.Errorf("graduated %v", got)
		}
	})
	t.Run("a grilling or a prototype is HITL at depth 1, human-free or not", func(t *testing.T) {
		f, g := graduating(t)
		grilling, prototype := gproposal("Ask the human", "grilling", ""), gproposal("Sketch it", "prototype", "")
		grilling.HumanFree, prototype.HumanFree = true, true
		f.graduator.Script(proposed(grilling, prototype))
		f.claude.Script(f.researched("research-1", findingsPath, findings("2026-10-12", true), "", answered))
		f.mustFire(0)
		if len(g.created) != 2 || strings.Join(g.created[0].Labels, ",") != "wayfinder:grilling,HITL" || strings.Join(g.created[1].Labels, ",") != "wayfinder:prototype,HITL" || len(g.labelled) != 0 {
			t.Errorf("created %+v, labelled %v", g.created, g.labelled)
		}
	})

	// Behaviour 3.
	t.Run("over 3 proposals: corrected once, then a warning and nothing created; the ticket stays resolved", func(t *testing.T) {
		f, g := graduating(t)
		four := proposed(gproposal("a", "research", ""), gproposal("b", "research", ""), gproposal("c", "research", ""), gproposal("d", "research", ""))
		f.graduator.Script(four)
		f.claude.Script(f.researched("research-1", findingsPath, findings("2026-10-12", true), "", answered))
		f.mustFire(0)
		calls := f.graduator.Calls()
		if len(calls) != 2 {
			t.Fatalf("%d calls, want the Step and its one correction", len(calls))
		}
		if resume, _ := calls[1].Arg("--resume"); resume != "graduate-1" || !strings.Contains(calls[1].Stdin, "proposes 4 tickets; at most 3") {
			t.Errorf("the correction resumed %q with %q", resume, calls[1].Stdin)
		}
		record := f.record()
		if len(g.created) != 0 || graduatedOf(record) != nil || !strings.Contains(fmt.Sprint(record["warnings"]), "the Graduate fog Step ended MALFORMED, so no fog is graduated") {
			t.Errorf("created %+v, record %v", g.created, record)
		}
		if f.labelled() != "AFK:done" || f.research.ticketState != "closed" || record["phase"] != "done" || resolveOf(record)["resolved"] != true || record["error"] != "" {
			t.Errorf("the ticket is %s, %q; record %v", f.research.ticketState, f.labelled(), record)
		}
		// The Map still gets its line; the fog stays.
		if !strings.Contains(f.research.mapBody, "issues/20): ") || !strings.Contains(f.research.mapBody, "- Which board the Daemon watches\n") {
			t.Errorf("the Map is\n%s", f.research.mapBody)
		}
	})
	t.Run("a corrected result within the bound is created", func(t *testing.T) {
		f, g := graduating(t)
		f.graduator.Script(
			proposed(gproposal("a", "research", ""), gproposal("b", "research", ""), gproposal("c", "research", ""), gproposal("d", "research", "")),
			proposed(gproposal("a", "research", "")),
		)
		f.claude.Script(f.researched("research-1", findingsPath, findings("2026-10-12", true), "", answered))
		f.mustFire(0)
		if len(g.created) != 1 || len(f.graduator.Calls()) != 2 {
			t.Errorf("created %+v after %d calls", g.created, len(f.graduator.Calls()))
		}
	})

	t.Run("cleared fog text that is not in the Map is not edited, and is a warning; the ticket is still created", func(t *testing.T) {
		f, g := graduating(t)
		f.graduator.Script(proposed(gproposal("a", "research", "Webhooks.")))
		f.claude.Script(f.researched("research-1", findingsPath, findings("2026-10-12", true), "", answered))
		f.mustFire(0)
		line := "- [Which merge gate?](https://github.com/acme/widgets/issues/20): A research PR is merged by the docs-only gate once CI is green."
		if want := strings.Replace(fogMapBody, "issues/3): old\n", "issues/3): old\n"+line+"\n", 1); f.research.mapBody != want {
			t.Errorf("the Map is\n%s", f.research.mapBody)
		}
		if len(g.created) != 1 || !strings.Contains(fmt.Sprint(f.record()["warnings"]), `#40 clears "Webhooks.", which is not in Map #9's Not yet specified, so the fog was not edited`) {
			t.Errorf("created %+v, warnings %v", g.created, f.record()["warnings"])
		}
	})
	t.Run("an AFK ticket that cannot be made the Map's sub-issue is HITL, and never on the pick signal", func(t *testing.T) {
		f, g := graduating(t)
		g.refuseSubIssue = true
		f.graduator.Script(proposed(gproposal("a", "research", "")))
		f.claude.Script(f.researched("research-1", findingsPath, findings("2026-10-12", true), "", answered))
		f.mustFire(0)
		if strings.Join(g.labelled, ",") != "#40 HITL" || !strings.Contains(fmt.Sprint(f.record()["warnings"]), "#40 could not be made a sub-issue of Map #9") {
			t.Errorf("labelled %v, warnings %v", g.labelled, f.record()["warnings"])
		}
		if got := graduatedOf(f.record()); len(got) != 1 || got[0]["route"] != "HITL" || got[0]["wired"] != false || got[0]["picked"] != false {
			t.Errorf("graduated %v", got)
		}
	})
	t.Run("a Step that fails is a warning; the ticket stays resolved and the Map gets its line", func(t *testing.T) {
		f, g := graduating(t)
		f.graduator.Script(steptest.Reply{ExitCode: 1, Stderr: "boom"})
		f.claude.Script(f.researched("research-1", findingsPath, findings("2026-10-12", true), "", answered))
		f.mustFire(0)
		record := f.record()
		if len(g.created) != 0 || !strings.Contains(fmt.Sprint(record["warnings"]), "the Graduate fog Step ended FAILED") || resolveOf(record)["resolved"] != true ||
			!strings.Contains(f.research.mapBody, "issues/20): ") {
			t.Errorf("created %+v, record %v", g.created, record)
		}
	})
	t.Run("a proposal of nothing creates nothing, and the Map gets only its line", func(t *testing.T) {
		f, g := graduating(t)
		f.claude.Script(f.researched("research-1", findingsPath, findings("2026-10-12", true), "", answered))
		f.mustFire(0)
		if got := graduationWrites(f); got != "PATCH /repos/acme/widgets/issues/9" || len(g.created) != 0 || len(f.graduator.Calls()) != 1 {
			t.Errorf("after the resolve GitHub received\n%s", got)
		}
	})
	t.Run("an AFK ticket the pick signal refuses is HITL, with a warning", func(t *testing.T) {
		f, g := graduating(t)
		f.gh.Handle("GET", "/repos/acme/widgets/labels", githubtest.Text(500, `{"message":"boom"}`))
		f.graduator.Script(proposed(gproposal("a", "research", "")))
		f.claude.Script(f.researched("research-1", findingsPath, findings("2026-10-12", true), "", answered))
		f.mustFire(0)
		if strings.Join(g.labelled, ",") != "#40 HITL" || !strings.Contains(fmt.Sprint(f.record()["warnings"]), "#40 was not put on the pick signal, so it is HITL") {
			t.Errorf("labelled %v, warnings %v", g.labelled, f.record()["warnings"])
		}
		if got := graduatedOf(f.record()); len(got) != 1 || got[0]["route"] != "HITL" || got[0]["wired"] != true || got[0]["picked"] != false {
			t.Errorf("graduated %v", got)
		}
	})
}
