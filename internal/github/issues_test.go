package github

import (
	"reflect"
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
}
