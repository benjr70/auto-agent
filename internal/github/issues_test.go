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
	t.Run("the open issues carrying a label are read page by page until a short one, pull requests on a full page counted", func(t *testing.T) {
		s := githubtest.New(t)
		full := make([]map[string]any, 0, 100)
		for i := 0; i < 99; i++ {
			full = append(full, map[string]any{"number": 1000 - i, "title": "newer", "state": "open"})
		}
		full = append(full, map[string]any{"number": 900, "title": "a PR", "state": "open", "pull_request": map[string]any{}})
		s.Handle("GET", "/repos/acme/widgets/issues",
			githubtest.JSON(200, full),
			githubtest.JSON(200, []map[string]any{{"number": 5, "title": "Bootstrap", "state": "open"}}))
		got, err := newClient(s).OpenIssues(ctx, "acme", "widgets", "AFK")
		if err != nil || len(got) != 100 || got[99].Number != 5 {
			t.Fatalf("OpenIssues = %d issues, last %+v, %v", len(got), got[len(got)-1], err)
		}
		requests := s.Requests()
		if len(requests) != 2 || requests[0].Query.Get("page") != "1" || requests[1].Query.Get("page") != "2" || requests[1].Query.Get("labels") != "AFK" || requests[1].Query.Get("per_page") != "100" {
			t.Errorf("requests = %+v", requests)
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
	t.Run("an issue is read with its database id, which the sub-issue and dependency calls take", func(t *testing.T) {
		s := githubtest.New(t)
		s.Handle("GET", "/repos/acme/widgets/issues/13", githubtest.JSON(200, map[string]any{"id": 9001, "number": 13, "title": "t", "state": "open"}))
		issue, err := newClient(s).Issue(ctx, "acme", "widgets", 13)
		if err != nil || issue.ID != 9001 || issue.Number != 13 {
			t.Fatalf("Issue = %+v, %v", issue, err)
		}
	})
	t.Run("an issue's sub-issues are read page by page, open and closed", func(t *testing.T) {
		s := githubtest.New(t)
		full := make([]map[string]any, 100)
		for i := range full {
			full[i] = map[string]any{"id": 1000 + i, "number": 100 + i, "state": "open"}
		}
		s.HandleFunc("GET", "/repos/acme/widgets/issues/9/sub_issues", func(r githubtest.Request) githubtest.Reply {
			if r.Query.Get("page") == "1" {
				return githubtest.JSON(200, full)
			}
			return githubtest.JSON(200, []map[string]any{{"id": 7, "number": 21, "title": "done", "state": "closed", "labels": []map[string]any{{"name": "wayfinder:task"}}}})
		})
		got, err := newClient(s).SubIssues(ctx, "acme", "widgets", 9)
		if err != nil || len(got) != 101 || !reflect.DeepEqual(got[100], Issue{ID: 7, Number: 21, Title: "done", State: "closed", Labels: []string{"wayfinder:task"}}) {
			t.Fatalf("SubIssues = %d issues, last %+v, %v", len(got), got[len(got)-1], err)
		}
		if q := s.Requests()[0].Query; q.Get("per_page") != "100" {
			t.Errorf("query = %v", q)
		}
	})
	t.Run("a sub-issue is added by its database id", func(t *testing.T) {
		s := githubtest.New(t)
		s.Handle("POST", "/repos/acme/widgets/issues/9/sub_issues", githubtest.JSON(201, map[string]any{"number": 9}))
		if err := newClient(s).AddSubIssue(ctx, "acme", "widgets", 9, 9001); err != nil {
			t.Fatal(err)
		}
		if writes := s.Writes(); len(writes) != 1 || writes[0].Body != `{"sub_issue_id":9001}` {
			t.Errorf("writes = %+v", writes)
		}
	})
	t.Run("a native blocked-by edge is added on the blocked issue, by the blocker's database id", func(t *testing.T) {
		s := githubtest.New(t)
		s.Handle("POST", "/repos/acme/widgets/issues/31/dependencies/blocked_by", githubtest.JSON(201, map[string]any{"number": 31}))
		if err := newClient(s).AddBlockedBy(ctx, "acme", "widgets", 31, 9001); err != nil {
			t.Fatal(err)
		}
		if writes := s.Writes(); len(writes) != 1 || writes[0].Body != `{"issue_id":9001}` {
			t.Errorf("writes = %+v", writes)
		}
	})
}
