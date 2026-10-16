package github

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/benjr70/auto-agent/internal/github/githubtest"
)

// threadsData is a reviewThreads answer.
func threadsData(next string, nodes ...map[string]any) githubtest.Reply {
	return githubtest.JSON(200, map[string]any{"data": map[string]any{"repository": map[string]any{"pullRequest": map[string]any{"reviewThreads": map[string]any{
		"pageInfo": map[string]any{"hasNextPage": next != "", "endCursor": next}, "nodes": nodes,
	}}}}})
}

// node is one thread of a reviewThreads answer; more is the cursor of its
// next page of comments, empty when it has none.
func node(id string, resolved bool, path string, line any, more string, comments ...map[string]any) map[string]any {
	return map[string]any{"id": id, "isResolved": resolved, "path": path, "line": line,
		"comments": map[string]any{"pageInfo": map[string]any{"hasNextPage": more != "", "endCursor": more}, "nodes": comments}}
}

func comment(id int64, body string) map[string]any {
	return map[string]any{"databaseId": id, "body": body, "author": map[string]any{"login": "machine-user"}}
}

// variables are the variables a GraphQL request was sent with.
func variables(t *testing.T, r githubtest.Request) map[string]any {
	t.Helper()
	var sent struct {
		Query     string
		Variables map[string]any
	}
	if err := json.Unmarshal([]byte(r.Body), &sent); err != nil {
		t.Fatal(err)
	}
	return sent.Variables
}

// Ported from lib/thread-reconciler.test.sh: the GitHub half of the Thread
// Reconciler. Who wrote what is the thread package's.
func TestReviewThreads(t *testing.T) {
	t.Run("enumerates only unresolved threads, distilled for the fix loop", func(t *testing.T) {
		s := githubtest.New(t)
		s.HandleGraphQL("reviewThreads", threadsData("",
			node("RT_1", false, "src/a.ts", 12, "", comment(9001, "rename this variable")),
			node("RT_2", true, "src/b.ts", 3, "", comment(9002, "already handled")),
			node("RT_3", false, "", nil, "", comment(9003, "missing test for the sad path")),
		))
		threads, err := newClient(s).ReviewThreads(ctx, "acme", "widgets", 310)
		if err != nil || len(threads) != 2 || threads[0].ID != "RT_1" || threads[0].Line != 12 || threads[0].Comments[0] != (ThreadComment{ID: 9001, Author: "machine-user", Body: "rename this variable"}) || threads[1].ID != "RT_3" || threads[1].Line != 0 {
			t.Fatalf("threads = %+v, err = %v", threads, err)
		}
		requests := s.Requests()
		if v := variables(t, requests[0]); v["owner"] != "acme" || v["name"] != "widgets" || v["pr"] != 310.0 {
			t.Errorf("the query targets the configured owner/name and the PR: %v", v)
		}
		if len(requests) != 1 {
			t.Errorf("a thread with one page triggers no follow-up call: %d requests", len(requests))
		}
		if !strings.Contains(requests[0].Body, "comments(first: 100)") || !strings.Contains(requests[0].Body, "pageInfo { hasNextPage endCursor }") {
			t.Error("the query asks for the whole thread, not only its first comment, with its pageInfo")
		}
	})
	t.Run("a thread longer than one page is followed to its end before replies/ruling are derived", func(t *testing.T) {
		s := githubtest.New(t)
		s.HandleGraphQL("reviewThreads", threadsData("",
			node("RT_LONG", false, "src/a.ts", 1, "C1", comment(9201, "first page"), comment(9202, "page one")),
			node("RT_SHORT", false, "src/b.ts", 2, "", comment(9203, "one page")),
			node("RT_DONE", true, "src/c.ts", 3, "Z", comment(9204, "resolved; never followed")),
		))
		page := func(next string, comments ...map[string]any) githubtest.Reply {
			return githubtest.JSON(200, map[string]any{"data": map[string]any{"node": map[string]any{"comments": map[string]any{
				"pageInfo": map[string]any{"hasNextPage": next != "", "endCursor": next}, "nodes": comments}}}})
		}
		s.HandleGraphQL("node(id:", page("C2", comment(9205, "please also handle the empty case"), comment(9206, "page two")), page("", comment(9207, "good, but revert page one")))
		threads, err := newClient(s).ReviewThreads(ctx, "acme", "widgets", 310)
		if err != nil || len(threads) != 2 || threads[0].ID != "RT_LONG" || threads[1].ID != "RT_SHORT" {
			t.Fatalf("threads = %+v, err = %v", threads, err)
		}
		var ids []int64
		for _, c := range threads[0].Comments {
			ids = append(ids, c.ID)
		}
		if len(ids) != 5 || ids[4] != 9207 {
			t.Errorf("replies span every page in order: %v", ids)
		}
		requests := s.Requests()
		if len(requests) != 3 || variables(t, requests[1])["after"] != "C1" || variables(t, requests[2])["after"] != "C2" || variables(t, requests[2])["id"] != "RT_LONG" {
			t.Errorf("each further page is fetched by thread id and cursor, only for unresolved threads with hasNextPage: %+v", requests)
		}
	})
	t.Run("the threads of a PR with more than one page of them are all read", func(t *testing.T) {
		s := githubtest.New(t)
		s.HandleGraphQL("reviewThreads", threadsData("T1", node("RT_A", false, "a", 1, "", comment(1, "a"))), threadsData("", node("RT_B", false, "b", 2, "", comment(2, "b"))))
		threads, err := newClient(s).ReviewThreads(ctx, "acme", "widgets", 310)
		if err != nil || len(threads) != 2 || threads[1].ID != "RT_B" || variables(t, s.Requests()[1])["after"] != "T1" {
			t.Errorf("threads = %+v, err = %v", threads, err)
		}
	})
	t.Run("an API failure exits non-zero, never bogus JSON", func(t *testing.T) {
		s := githubtest.New(t)
		s.HandleGraphQL("reviewThreads", githubtest.Text(502, "bad gateway"))
		if threads, err := newClient(s).ReviewThreads(ctx, "acme", "widgets", 310); err == nil || threads != nil {
			t.Errorf("threads = %v, err = %v", threads, err)
		}
	})
	t.Run("reply lands in-thread via in_reply_to on the configured repo", func(t *testing.T) {
		s := githubtest.New(t)
		s.Handle("POST", "/repos/acme/widgets/pulls/310/comments", githubtest.JSON(201, map[string]any{"id": 1}))
		if err := newClient(s).ReplyToThread(ctx, "acme", "widgets", 310, 9001, "<!-- auto-agent:fix -->\nfixed in abc123: renamed the variable"); err != nil {
			t.Fatal(err)
		}
		writes := s.Writes()
		var sent struct {
			Body      string
			InReplyTo int64 `json:"in_reply_to"`
		}
		if len(writes) != 1 || json.Unmarshal([]byte(writes[0].Body), &sent) != nil || sent.InReplyTo != 9001 || sent.Body != "<!-- auto-agent:fix -->\nfixed in abc123: renamed the variable" {
			t.Errorf("writes = %+v", writes)
		}
	})
	t.Run("resolve fires the resolveReviewThread mutation with the thread id", func(t *testing.T) {
		s := githubtest.New(t)
		s.HandleGraphQL("resolveReviewThread", githubtest.JSON(200, map[string]any{"data": map[string]any{}}))
		if err := newClient(s).ResolveThread(ctx, "RT_1"); err != nil {
			t.Fatal(err)
		}
		if writes := s.Writes(); len(writes) != 1 || variables(t, writes[0])["threadId"] != "RT_1" {
			t.Errorf("writes = %+v", writes)
		}
	})
}
