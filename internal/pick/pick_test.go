package pick

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/benjr70/auto-agent/internal/config"
	"github.com/benjr70/auto-agent/internal/github"
	"github.com/benjr70/auto-agent/internal/github/githubtest"
	"github.com/benjr70/auto-agent/internal/state"
)

// The cases of lib/pick-publish.test.sh, under their names, with publish
// as Add and unpublish as Remove. Add and Remove also put on and take off
// the AFK label, which pick-publish left to its callers.

func harness(shape string) config.Harness {
	h := config.Harness{Repo: config.Repo{Owner: "acme", Name: "widgets"}, Pick: config.Pick{Shape: shape}}
	if shape == config.PickProject {
		h.Pick.Project = &config.Project{Number: 7, PriorityField: "Priority", Order: []string{"P0", "P1", "P2"}}
	}
	return h
}

const fields = `{"id":"F_prio","options":[{"id":"O_p0","name":"P0"},{"id":"O_p1","name":"P1"},{"id":"O_p2","name":"P2"}]}`

func boardReply(field string) githubtest.Reply {
	return githubtest.Text(200, `{"data":{"repository":{"issue":{"id":"I_41"},"owner":{"projectV2":{"id":"PVT_proj7","field":`+field+`}}}}}`)
}

// fake is fakeBoard with board 7's Priority field, AFK labels that go on
// and come off, issue 41 added as PVTI_item41, and the given replies to
// the Priority edit and the item delete.
func fake(t *testing.T, edit, del githubtest.Reply) (*githubtest.Server, *github.Client) {
	s, client := fakeBoard(t, boardReply(fields))
	s.Handle("POST", "/repos/acme/widgets/issues/41/labels", githubtest.Text(200, `[]`))
	s.Handle("DELETE", "/repos/acme/widgets/issues/41/labels/AFK", githubtest.Text(200, `[]`))
	s.Handle("DELETE", "/repos/acme/widgets/issues/99/labels/AFK", githubtest.JSON(404, map[string]string{"message": "Label does not exist"}))
	s.HandleGraphQL("addProjectV2ItemById", githubtest.Text(200, `{"data":{"addProjectV2ItemById":{"item":{"id":"PVTI_item41"}}}}`))
	s.HandleGraphQL("updateProjectV2ItemFieldValue", edit)
	s.HandleGraphQL("deleteProjectV2Item", del)
	return s, client
}

var (
	editOK   = githubtest.Text(200, `{"data":{"updateProjectV2ItemFieldValue":{"projectV2Item":{"id":"PVTI_item41"}}}}`)
	deleteOK = githubtest.Text(200, `{"data":{"deleteProjectV2Item":{"deletedItemId":"PVTI_item41"}}}`)
)

func itemsReply(issue string) githubtest.Reply {
	return githubtest.Text(200, `{"data":{"repository":{"issue":`+issue+`,"owner":{"projectV2":{"id":"PVT_proj7"}}}}}`)
}

// writes are the requests that changed something, as "METHOD path" or, for
// a mutation, its name and variables.
func writes(t *testing.T, s *githubtest.Server) []string {
	t.Helper()
	var out []string
	for _, w := range s.Writes() {
		if w.Path != "/graphql" {
			out = append(out, w.Method+" "+w.Path+" "+strings.TrimSpace(w.Body))
			continue
		}
		var body struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		json.Unmarshal([]byte(w.Body), &body)
		name := strings.Fields(strings.SplitN(body.Query, "{", 3)[1])[0]
		vars, _ := json.Marshal(body.Variables)
		out = append(out, strings.TrimSuffix(name, "(input:")+" "+string(vars))
	}
	return out
}

func reason(err error) string {
	var e *Error
	if errors.As(err, &e) {
		return e.Reason
	}
	return ""
}

func TestAdd(t *testing.T) {
	ctx := context.Background()
	t.Run("project pick: publish adds the item and sets the asked Priority (AC 2)", func(t *testing.T) {
		s, client := fake(t, editOK, deleteOK)
		got, err := Add(ctx, client, harness(config.PickProject), 41, "P1")
		t.Run("exit 0 with a projected verdict", func(t *testing.T) {
			if err != nil || got.Shape != "project" || !got.OnBoard || got.Priority != "P1" || got.ItemID != "PVTI_item41" {
				t.Errorf("Add = %+v, %v", got, err)
			}
		})
		w := writes(t, s)
		t.Run("item-add ran against the configured project, owner and issue url", func(t *testing.T) {
			if len(w) < 1 || w[0] != `addProjectV2ItemById {"content":"I_41","project":"PVT_proj7"}` {
				t.Errorf("writes = %q", w)
			}
		})
		t.Run("item-edit ran with the project, item, Priority field and P1 option ids", func(t *testing.T) {
			if len(w) < 2 || w[1] != `updateProjectV2ItemFieldValue {"field":"F_prio","item":"PVTI_item41","option":"O_p1","project":"PVT_proj7"}` {
				t.Errorf("writes = %q", w)
			}
		})
		t.Run("the AFK label goes on last, once the board write landed", func(t *testing.T) {
			if len(w) != 3 || w[2] != `POST /repos/acme/widgets/issues/41/labels {"labels":["AFK"]}` {
				t.Errorf("writes = %q", w)
			}
		})
	})
	t.Run("project pick: no --priority means the first value of the configured order", func(t *testing.T) {
		// pick-publish defaulted to the last value; the Spec's helper
		// defaults to the first (issue #177).
		s, client := fake(t, editOK, deleteOK)
		got, err := Add(ctx, client, harness(config.PickProject), 41, "")
		if w := writes(t, s); err != nil || got.Priority != "P0" || len(w) < 2 || !strings.Contains(w[1], `"option":"O_p0"`) {
			t.Errorf("Add = %+v, %v; writes %q", got, err, w)
		}
	})
	t.Run("project pick: a failed item-edit is a failure, never a silent downgrade", func(t *testing.T) {
		s, client := fake(t, githubtest.Text(200, `{"errors":[{"message":"no"}]}`), deleteOK)
		got, err := Add(ctx, client, harness(config.PickProject), 41, "P0")
		w := writes(t, s)
		if reason(err) != ReasonPriorityEditFailed || !got.OnBoard || got.Priority != "" {
			t.Errorf("Add = %+v, %v", got, err)
		}
		for _, write := range w {
			if strings.Contains(write, "/labels") {
				t.Errorf("AFK was put on a ticket whose Priority did not land: %q", w)
			}
		}
	})
	t.Run("project pick: a Priority outside the configured order is refused before any write", func(t *testing.T) {
		s, client := fake(t, editOK, deleteOK)
		_, err := Add(ctx, client, harness(config.PickProject), 41, "P9")
		if reason(err) != ReasonUnknownPriority || len(s.Requests()) != 0 {
			t.Errorf("Add = %v; requests %d", err, len(s.Requests()))
		}
	})
	t.Run("project pick: a Priority the board has no option for fails with option-missing", func(t *testing.T) {
		s, client := fakeBoard(t, boardReply(`{"id":"F_prio","options":[{"id":"O_p2","name":"P2"}]}`))
		_, err := Add(ctx, client, harness(config.PickProject), 41, "P1")
		if reason(err) != ReasonOptionMissing || len(writes(t, s)) != 0 {
			t.Errorf("Add = %v; writes %q", err, writes(t, s))
		}
	})
	t.Run("project pick: a board without the priority field fails with field-missing", func(t *testing.T) {
		s, client := fakeBoard(t, boardReply(`{}`))
		_, err := Add(ctx, client, harness(config.PickProject), 41, "P1")
		if reason(err) != ReasonFieldMissing || len(writes(t, s)) != 0 {
			t.Errorf("Add = %v; writes %q", err, writes(t, s))
		}
	})
	t.Run("project pick: an unreadable board or issue fails before any write", func(t *testing.T) {
		for name, tc := range map[string]struct {
			reply githubtest.Reply
			want  string
		}{
			"no project":   {githubtest.Text(200, `{"data":{"repository":{"issue":{"id":"I_41"},"owner":{"projectV2":null}}},"errors":[{"message":"Could not resolve to a ProjectV2"}]}`), ReasonProjectUnreadable},
			"no issue":     {githubtest.Text(200, `{"data":{"repository":{"issue":null,"owner":{"projectV2":{"id":"P"}}}},"errors":[{"message":"Could not resolve to an Issue"}]}`), ReasonIssueUnreadable},
			"GitHub fails": {githubtest.Text(502, `bad gateway`), ReasonProjectUnreadable},
		} {
			t.Run(name, func(t *testing.T) {
				s, client := fakeBoard(t, tc.reply)
				_, err := Add(ctx, client, harness(config.PickProject), 41, "P1")
				if reason(err) != tc.want || len(writes(t, s)) != 0 {
					t.Errorf("Add = %v; writes %q", err, writes(t, s))
				}
			})
		}
	})
	t.Run("project pick: a failed item-add is a failure and no label goes on", func(t *testing.T) {
		s, client := fakeBoard(t, boardReply(fields))
		s.HandleGraphQL("addProjectV2ItemById", githubtest.Text(200, `{"errors":[{"message":"Resource not accessible"}]}`))
		got, err := Add(ctx, client, harness(config.PickProject), 41, "P1")
		if reason(err) != ReasonItemAddFailed || got.OnBoard || len(writes(t, s)) != 1 {
			t.Errorf("Add = %+v, %v; writes %q", got, err, writes(t, s))
		}
	})
	t.Run("label-only pick: publish projects nothing and touches no gh project command (AC 2)", func(t *testing.T) {
		s, client := fake(t, editOK, deleteOK)
		got, err := Add(ctx, client, harness(config.PickLabels), 41, "P1")
		t.Run("exit 0, shape labels, projected false, priority null", func(t *testing.T) {
			if err != nil || got.Shape != "labels" || got.OnBoard || got.Priority != "" {
				t.Errorf("Add = %+v, %v", got, err)
			}
		})
		t.Run("no gh call at all", func(t *testing.T) {
			// Now: the labels read, and the one AFK label write.
			for _, r := range s.Requests() {
				if r.Path == "/graphql" {
					t.Errorf("a project call was made: %s", r.Body)
				}
			}
			if w := writes(t, s); len(w) != 1 || w[0] != `POST /repos/acme/widgets/issues/41/labels {"labels":["AFK"]}` {
				t.Errorf("writes = %q", w)
			}
		})
	})
	t.Run("labels are ensured before the AFK label goes on", func(t *testing.T) {
		s := githubtest.New(t)
		s.Handle("GET", "/repos/acme/widgets/labels", githubtest.JSON(200, []map[string]string{}))
		s.Handle("POST", "/repos/acme/widgets/labels", githubtest.JSON(201, map[string]string{}))
		s.Handle("POST", "/repos/acme/widgets/issues/41/labels", githubtest.Text(200, `[]`))
		got, err := Add(ctx, github.New(github.Options{BaseURL: s.URL, Token: "t"}), harness(config.PickLabels), 41, "")
		w := writes(t, s)
		if err != nil || len(got.LabelsCreated) != len(state.HarnessLabels) || len(w) != len(state.HarnessLabels)+1 || !strings.HasPrefix(w[len(w)-1], "POST /repos/acme/widgets/issues/41/labels") {
			t.Errorf("Add = %+v, %v; writes %q", got, err, w)
		}
	})
	t.Run("labels that cannot be ensured stop the add before any write to the ticket", func(t *testing.T) {
		s := githubtest.New(t)
		s.Handle("GET", "/repos/acme/widgets/labels", githubtest.Text(500, "boom"))
		_, err := Add(ctx, github.New(github.Options{BaseURL: s.URL, Token: "t"}), harness(config.PickLabels), 41, "")
		if reason(err) != ReasonLabelsFailed || len(s.Writes()) != 0 {
			t.Errorf("Add = %v; writes %d", err, len(s.Writes()))
		}
	})
	t.Run("an AFK label GitHub refuses is a failure", func(t *testing.T) {
		s, client := fake(t, editOK, deleteOK)
		s.Handle("POST", "/repos/acme/widgets/issues/41/labels", githubtest.JSON(403, map[string]string{"message": "Must have push access"}))
		if _, err := Add(ctx, client, harness(config.PickLabels), 41, ""); reason(err) != ReasonLabelFailed {
			t.Errorf("Add = %v", err)
		}
	})
}

// fakeBoard is a GitHub with every harness label present and the board
// query answered with reply.
func fakeBoard(t *testing.T, reply githubtest.Reply) (*githubtest.Server, *github.Client) {
	s := githubtest.New(t)
	var labels []map[string]string
	for _, l := range state.HarnessLabels {
		labels = append(labels, map[string]string{"name": l.Name})
	}
	s.Handle("GET", "/repos/acme/widgets/labels", githubtest.JSON(200, labels))
	s.HandleGraphQL("field(name: $field)", reply)
	return s, github.New(github.Options{BaseURL: s.URL, Token: "t"})
}

func TestRemove(t *testing.T) {
	ctx := context.Background()
	const onBoards = `{"projectItems":{"nodes":[{"id":"PVTI_other","project":{"id":"PVT_proj3"}},{"id":"PVTI_item41","project":{"id":"PVT_proj7"}}],"pageInfo":{"hasNextPage":false}}}`
	t.Run("project pick: unpublish finds the issue's item and deletes it", func(t *testing.T) {
		s, client := fake(t, editOK, deleteOK)
		s.HandleGraphQL("projectItems(", itemsReply(onBoards))
		got, err := Remove(ctx, client, harness(config.PickProject), 41)
		t.Run("exit 0 with removed true and the item id", func(t *testing.T) {
			if err != nil || got.Shape != "project" || !got.OnBoard || got.ItemID != "PVTI_item41" {
				t.Errorf("Remove = %+v, %v", got, err)
			}
		})
		t.Run("item-delete ran on the right item and never on the other", func(t *testing.T) {
			w := writes(t, s)
			if len(w) != 2 || w[1] != `deleteProjectV2Item {"item":"PVTI_item41","project":"PVT_proj7"}` {
				t.Errorf("writes = %q", w)
			}
		})
		t.Run("the AFK label comes off first", func(t *testing.T) {
			if w := writes(t, s); len(w) == 0 || !strings.HasPrefix(w[0], "DELETE /repos/acme/widgets/issues/41/labels/AFK") {
				t.Errorf("writes = %q", w)
			}
		})
	})
	t.Run("project pick: unpublish of an issue not on the board is a no-op success", func(t *testing.T) {
		s, client := fake(t, editOK, deleteOK)
		s.HandleGraphQL("projectItems(", itemsReply(`{"projectItems":{"nodes":[{"id":"PVTI_other","project":{"id":"PVT_proj3"}}],"pageInfo":{"hasNextPage":false}}}`))
		got, err := Remove(ctx, client, harness(config.PickProject), 99)
		if w := writes(t, s); err != nil || got.OnBoard || len(w) != 1 {
			t.Errorf("Remove = %+v, %v; writes %q", got, err, w)
		}
	})
	t.Run("project pick: an issue on more boards than one read returns cannot be proven absent", func(t *testing.T) {
		s, client := fake(t, editOK, deleteOK)
		s.HandleGraphQL("projectItems(", itemsReply(`{"projectItems":{"nodes":[{"id":"PVTI_other","project":{"id":"PVT_proj3"}}],"pageInfo":{"hasNextPage":true}}}`))
		if _, err := Remove(ctx, client, harness(config.PickProject), 41); reason(err) != ReasonItemListTruncated {
			t.Errorf("Remove = %v", err)
		}
	})
	t.Run("project pick: unreadable boards and a refused delete are failures", func(t *testing.T) {
		s, client := fake(t, editOK, deleteOK)
		s.HandleGraphQL("projectItems(", githubtest.Text(502, "bad gateway"))
		if _, err := Remove(ctx, client, harness(config.PickProject), 41); reason(err) != ReasonItemListUnreadable {
			t.Errorf("Remove = %v", err)
		}
		s, client = fake(t, editOK, githubtest.Text(200, `{"errors":[{"message":"no"}]}`))
		s.HandleGraphQL("projectItems(", itemsReply(onBoards))
		if got, err := Remove(ctx, client, harness(config.PickProject), 41); reason(err) != ReasonItemDeleteFailed || got.OnBoard {
			t.Errorf("Remove = %+v, %v", got, err)
		}
	})
	t.Run("label-only pick: unpublish is a no-op", func(t *testing.T) {
		// Now: the AFK label comes off, and nothing else is asked.
		s, client := fake(t, editOK, deleteOK)
		got, err := Remove(ctx, client, harness(config.PickLabels), 41)
		if w := writes(t, s); err != nil || got.OnBoard || len(s.Requests()) != 1 || len(w) != 1 || !strings.HasPrefix(w[0], "DELETE /repos/acme/widgets/issues/41/labels/AFK") {
			t.Errorf("Remove = %+v, %v; requests %d", got, err, len(s.Requests()))
		}
	})
	t.Run("a ticket without the AFK label is already off it", func(t *testing.T) {
		_, client := fake(t, editOK, deleteOK)
		if _, err := Remove(ctx, client, harness(config.PickLabels), 99); err != nil {
			t.Errorf("Remove = %v", err)
		}
	})
}
