package github

import (
	"reflect"
	"strings"
	"testing"

	"github.com/benjr70/auto-agent/internal/github/githubtest"
)

func TestIssues(t *testing.T) {
	t.Run("the open issues carrying a label are read with their bodies and who opened them, pull requests left out", func(t *testing.T) {
		s := githubtest.New(t)
		s.Handle("GET", "/repos/acme/widgets/issues", githubtest.JSON(200, []map[string]any{
			{"number": 52, "title": "parked", "body": "<!-- marker -->", "state": "open", "user": map[string]any{"login": "machine-user"}, "labels": []map[string]any{{"name": "AFK:needs-human"}}},
			{"number": 53, "title": "a PR", "state": "open", "pull_request": map[string]any{}},
			{"number": 54, "title": "no author", "body": nil, "state": "open", "user": nil},
		}))
		got, err := newClient(s).OpenIssues(ctx, "acme", "widgets", "AFK:needs-human")
		want := []Issue{
			{Number: 52, Title: "parked", Body: "<!-- marker -->", State: "open", Author: "machine-user", Labels: []string{"AFK:needs-human"}},
			{Number: 54, Title: "no author", State: "open"},
		}
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("OpenIssues = %+v, %v", got, err)
		}
		if q := s.Requests()[0].Query; q.Get("state") != "open" || q.Get("labels") != "AFK:needs-human" {
			t.Errorf("query = %v", q)
		}
	})
	t.Run("an issue is opened with its labels, and its number read back", func(t *testing.T) {
		s := githubtest.New(t)
		s.Handle("POST", "/repos/acme/widgets/issues", githubtest.JSON(201, map[string]any{"number": 77, "title": "t", "state": "open"}))
		issue, err := newClient(s).CreateIssue(ctx, "acme", "widgets", NewIssue{Title: "t", Body: "b", Labels: []string{"AFK:needs-human"}})
		if err != nil || issue.Number != 77 {
			t.Fatalf("CreateIssue = %+v, %v", issue, err)
		}
		if writes := s.Writes(); len(writes) != 1 || writes[0].Body != `{"title":"t","body":"b","labels":["AFK:needs-human"]}` {
			t.Errorf("writes = %+v", writes)
		}
	})
	t.Run("closing an issue patches its state, as completed", func(t *testing.T) {
		s := githubtest.New(t)
		s.Handle("PATCH", "/repos/acme/widgets/issues/77", githubtest.JSON(200, map[string]any{"number": 77}))
		if err := newClient(s).CloseIssue(ctx, "acme", "widgets", 77); err != nil {
			t.Fatal(err)
		}
		if writes := s.Writes(); len(writes) != 1 || writes[0].Body != `{"state":"closed","state_reason":"completed"}` {
			t.Errorf("writes = %+v", writes)
		}
	})
	t.Run("an issue's body is replaced whole, and nothing else is sent", func(t *testing.T) {
		s := githubtest.New(t)
		s.Handle("PATCH", "/repos/acme/widgets/issues/9", githubtest.JSON(200, map[string]any{"number": 9}))
		if err := newClient(s).EditIssueBody(ctx, "acme", "widgets", 9, "## Decisions so far\n"); err != nil {
			t.Fatal(err)
		}
		if writes := s.Writes(); len(writes) != 1 || writes[0].Body != `{"body":"## Decisions so far\n"}` {
			t.Errorf("writes = %+v", writes)
		}
	})
	t.Run("an issue is assigned by login", func(t *testing.T) {
		s := githubtest.New(t)
		s.Handle("POST", "/repos/acme/widgets/issues/13/assignees", githubtest.JSON(201, map[string]any{"number": 13}))
		if err := newClient(s).AddAssignees(ctx, "acme", "widgets", 13, "machine-user"); err != nil {
			t.Fatal(err)
		}
		if writes := s.Writes(); len(writes) != 1 || writes[0].Body != `{"assignees":["machine-user"]}` {
			t.Errorf("writes = %+v", writes)
		}
	})
	t.Run("an issue's GitHub parent is read with its title and body", func(t *testing.T) {
		s := githubtest.New(t)
		s.HandleGraphQL("parent { number title body }",
			githubtest.JSON(200, map[string]any{"data": map[string]any{"repository": map[string]any{"issue": map[string]any{"parent": map[string]any{"number": 9, "title": "Map", "body": "## Destination\n\nX"}}}}}),
			githubtest.JSON(200, map[string]any{"data": map[string]any{"repository": map[string]any{"issue": map[string]any{"parent": nil}}}}),
			githubtest.JSON(200, map[string]any{"data": map[string]any{"repository": map[string]any{"issue": nil}}}))
		c := newClient(s)
		parent, ok, err := c.Parent(ctx, "acme", "widgets", 13)
		if err != nil || !ok || !reflect.DeepEqual(parent, Issue{Number: 9, Title: "Map", Body: "## Destination\n\nX"}) {
			t.Fatalf("Parent = %+v, %v, %v", parent, ok, err)
		}
		if !strings.Contains(s.Requests()[0].Body, `"number":13`) {
			t.Errorf("the query asked about %s", s.Requests()[0].Body)
		}
		if _, ok, err := c.Parent(ctx, "acme", "widgets", 13); ok || err != nil {
			t.Errorf("an issue with no parent: ok %v, err %v", ok, err)
		}
		if _, _, err := c.Parent(ctx, "acme", "widgets", 13); err == nil {
			t.Error("an issue missing from the answer is not an error")
		}
	})
}
