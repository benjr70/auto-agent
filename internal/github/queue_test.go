package github

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/benjr70/auto-agent/internal/github/githubtest"
)

// queueReply wraps issue nodes in the pick query's response shape.
func queueReply(nodes ...string) githubtest.Reply {
	return githubtest.Text(200, `{"data":{"repository":{"issues":{"nodes":[`+strings.Join(nodes, ",")+`]}}}}`)
}

func sentQuery(t *testing.T, r githubtest.Request) (string, map[string]any) {
	t.Helper()
	var body struct {
		Query     string         `json:"query"`
		Variables map[string]any `json:"variables"`
	}
	if err := json.Unmarshal([]byte(r.Body), &body); err != nil {
		t.Fatal(err)
	}
	return body.Query, body.Variables
}

func TestQueue(t *testing.T) {
	const full = `{"number":10,"title":"T","createdAt":"2026-01-01T00:00:00Z",
		"labels":{"nodes":[{"name":"AFK"},{"name":"wayfinder:research"}]},
		"blockedBy":{"nodes":[{"number":98,"state":"CLOSED"},{"number":99,"state":"OPEN"}],"pageInfo":{"hasNextPage":true}},
		"assignees":{"nodes":[{"login":"machine-user"}]},
		"projectItems":{"nodes":[{"project":{"number":1},"fieldValueByName":{"name":"P1"}},{"project":{"number":9},"fieldValueByName":null},{"project":null}]}}`

	t.Run("the pick query names the configured owner, name and priority field", func(t *testing.T) {
		s := githubtest.New(t)
		s.Handle("POST", "/graphql", queueReply(full))
		issues, warnings, err := newClient(s).Queue(ctx, "acme", "widgets", "AFK", "Priority")
		if err != nil || len(warnings) != 0 {
			t.Fatalf("Queue: %v, warnings %q", err, warnings)
		}
		want := []QueueIssue{{
			Number: 10, Title: "T", CreatedAt: "2026-01-01T00:00:00Z", Labels: []string{"AFK", "wayfinder:research"},
			Blockers: []QueueBlocker{{98, "CLOSED"}, {99, "OPEN"}}, BlockersTruncated: true, Assignees: []string{"machine-user"},
			ProjectItems: []QueueProjectItem{{Project: 1, Priority: "P1"}, {Project: 9}},
		}}
		if !reflect.DeepEqual(issues, want) {
			t.Errorf("issues = %+v", issues)
		}
		query, variables := sentQuery(t, s.Requests()[0])
		wantVars := map[string]any{"owner": "acme", "name": "widgets", "label": "AFK", "field": "Priority"}
		if !reflect.DeepEqual(variables, wantVars) || !strings.Contains(query, "fieldValueByName(name: $field)") || !strings.Contains(query, "states: OPEN") {
			t.Errorf("variables = %v\nquery = %s", variables, query)
		}
	})
	t.Run("the pick reads no body: the query omits it and no issue-view follow-up", func(t *testing.T) {
		s := githubtest.New(t)
		s.Handle("POST", "/graphql", queueReply(full))
		newClient(s).Queue(ctx, "acme", "widgets", "AFK", "Priority")
		query, _ := sentQuery(t, s.Requests()[0])
		if strings.Contains(query, "body") || len(s.Requests()) != 1 {
			t.Errorf("requests = %d, query = %s", len(s.Requests()), query)
		}
	})
	t.Run("the label-only query selects no projectItems", func(t *testing.T) {
		s := githubtest.New(t)
		s.Handle("POST", "/graphql", queueReply())
		issues, _, err := newClient(s).Queue(ctx, "acme", "widgets", "AFK", "")
		query, variables := sentQuery(t, s.Requests()[0])
		if err != nil || len(issues) != 0 || strings.Contains(query, "projectItems") || strings.Contains(query, "$field") || !strings.Contains(query, "blockedBy") {
			t.Errorf("issues = %v, err = %v, query = %s", issues, err, query)
		}
		if _, sent := variables["field"]; sent {
			t.Errorf("variables = %v", variables)
		}
	})
	t.Run("config strings are JSON-escaped into the query", func(t *testing.T) {
		s := githubtest.New(t)
		s.Handle("POST", "/graphql", queueReply())
		newClient(s).Queue(ctx, "acme", `wid"gets`, "AFK", `Pri"ority`)
		query, variables := sentQuery(t, s.Requests()[0])
		if variables["name"] != `wid"gets` || variables["field"] != `Pri"ority` || strings.Contains(query, "gets") || strings.Contains(query, "ority") {
			t.Errorf("variables = %v\nquery = %s", variables, query)
		}
	})
	t.Run("null blockedBy/assignees: treated as empty, filter survives", func(t *testing.T) {
		s := githubtest.New(t)
		s.Handle("POST", "/graphql", githubtest.Text(200, `{"data":{"repository":{"issues":{"nodes":[
			{"number":10,"title":"partial-node","createdAt":"2026-01-01T00:00:00Z","labels":null,"blockedBy":{"nodes":null},"assignees":{"nodes":null},"projectItems":null},
			{"number":20,"title":"healthy","createdAt":"2026-02-01T00:00:00Z","labels":{"nodes":[{"name":"AFK"}]},"blockedBy":{"nodes":[],"pageInfo":{"hasNextPage":false}},"assignees":{"nodes":[]}}
		]}}},"errors":[{"type":"FORBIDDEN","message":"Resource not accessible by integration","path":["repository","issues","nodes",0,"projectItems"]}]}`))
		issues, warnings, err := newClient(s).Queue(ctx, "acme", "widgets", "AFK", "Priority")
		if err != nil || len(issues) != 2 || issues[0].Number != 10 || issues[0].Blockers != nil || issues[0].Assignees != nil || issues[0].BlockersTruncated {
			t.Fatalf("issues = %+v, err = %v", issues, err)
		}
		if !reflect.DeepEqual(warnings, []string{"Resource not accessible by integration"}) {
			t.Errorf("warnings = %q", warnings)
		}
	})
	t.Run("a response with no repository is an empty queue with its warning", func(t *testing.T) {
		s := githubtest.New(t)
		s.Handle("POST", "/graphql", githubtest.Text(200, `{"data":{"repository":null},"errors":[{"type":"NOT_FOUND","message":"Could not resolve to a Repository"}]}`))
		issues, warnings, err := newClient(s).Queue(ctx, "acme", "nope", "AFK", "")
		if err != nil || len(issues) != 0 || len(warnings) != 1 {
			t.Errorf("issues = %v, warnings = %q, err = %v", issues, warnings, err)
		}
	})
	t.Run("a broken GraphQL response is an error, not an empty queue", func(t *testing.T) {
		s := githubtest.New(t)
		s.Handle("POST", "/graphql", githubtest.Text(200, "garbage"), githubtest.Text(200, `{"errors":[{"message":"Parse error"}]}`), githubtest.Text(502, "bad gateway"))
		c := newClient(s)
		for i := 0; i < 3; i++ {
			if _, _, err := c.Queue(ctx, "acme", "widgets", "AFK", ""); err == nil {
				t.Errorf("reply %d gave no error", i)
			}
		}
		var gql *GraphQLError
		if _, _, err := c.Queue(ctx, "acme", "widgets", "AFK", ""); errors.As(err, &gql) {
			t.Errorf("a 502 is not a GraphQL error: %v", err)
		}
	})
}

func TestOpenIssuesLabelled(t *testing.T) {
	t.Run("issue list/view calls carry --repo acme/widgets", func(t *testing.T) {
		s := githubtest.New(t)
		s.Handle("GET", "/repos/acme/widgets/issues", githubtest.Text(200, `[{"number":7},{"number":8,"pull_request":{"url":"x"}},{"number":9}]`))
		got, err := newClient(s).OpenIssuesLabelled(ctx, "acme", "widgets", "AFK:in-progress")
		if err != nil || !reflect.DeepEqual(got, []int{7, 9}) {
			t.Errorf("OpenIssuesLabelled = %v, %v", got, err)
		}
		q := s.Requests()[0].Query
		if q.Get("labels") != "AFK:in-progress" || q.Get("state") != "open" || q.Get("per_page") != "100" {
			t.Errorf("query = %v", q)
		}
	})
	t.Run("no holder is an empty list, and a failure is an error", func(t *testing.T) {
		s := githubtest.New(t)
		s.Handle("GET", "/repos/acme/widgets/issues", githubtest.Text(200, `[]`), githubtest.Text(500, "boom"))
		c := newClient(s)
		if got, err := c.OpenIssuesLabelled(ctx, "acme", "widgets", "AFK:in-progress"); err != nil || got == nil || len(got) != 0 {
			t.Errorf("OpenIssuesLabelled = %#v, %v", got, err)
		}
		if _, err := c.OpenIssuesLabelled(ctx, "acme", "widgets", "AFK:in-progress"); err == nil {
			t.Error("a 500 gave no error")
		}
	})
}
