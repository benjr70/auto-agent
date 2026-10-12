package github

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/benjr70/auto-agent/internal/github/githubtest"
)

func TestPullRequests(t *testing.T) {
	pull := func(number int, ref, repo string) map[string]any {
		head := map[string]any{"ref": ref, "sha": "abc", "repo": nil}
		if repo != "" {
			head["repo"] = map[string]any{"full_name": repo}
		}
		return map[string]any{"number": number, "title": "t", "state": "open", "head": head, "base": map[string]any{"ref": "trunk"}}
	}
	t.Run("opening a pull request posts the title, head, base and body, and returns its number", func(t *testing.T) {
		s := githubtest.New(t)
		s.Handle("POST", "/repos/acme/widgets/pulls", githubtest.JSON(201, pull(12, "feat/issue-7", "acme/widgets")))
		pr, err := newClient(s).CreatePullRequest(ctx, "acme", "widgets", NewPullRequest{Title: "feat(a): thing", Head: "feat/issue-7", Base: "trunk", Body: "Closes #7"})
		if err != nil || pr.Number != 12 || pr.HeadRef != "feat/issue-7" || pr.HeadRepo != "acme/widgets" {
			t.Fatalf("pr = %+v, err = %v", pr, err)
		}
		var sent map[string]string
		if err := json.Unmarshal([]byte(s.Writes()[0].Body), &sent); err != nil {
			t.Fatal(err)
		}
		want := map[string]string{"title": "feat(a): thing", "head": "feat/issue-7", "base": "trunk", "body": "Closes #7"}
		if fmt.Sprint(sent) != fmt.Sprint(want) {
			t.Errorf("sent %v, want %v", sent, want)
		}
	})
	t.Run("a refused pull request is an error naming GitHub's reason", func(t *testing.T) {
		s := githubtest.New(t)
		s.Handle("POST", "/repos/acme/widgets/pulls", githubtest.JSON(422, map[string]any{"message": "Validation Failed"}))
		if _, err := newClient(s).CreatePullRequest(ctx, "acme", "widgets", NewPullRequest{}); err == nil || !strings.Contains(err.Error(), "Validation Failed") {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("the open pull requests are read page by page; a head whose repository is gone has none", func(t *testing.T) {
		s := githubtest.New(t)
		var full []map[string]any
		for i := 1; i <= 100; i++ {
			full = append(full, pull(i, fmt.Sprintf("b%d", i), "acme/widgets"))
		}
		s.Handle("GET", "/repos/acme/widgets/pulls", githubtest.JSON(200, full), githubtest.JSON(200, []map[string]any{pull(101, "gone", "")}))
		prs, err := newClient(s).OpenPullRequests(ctx, "acme", "widgets")
		if err != nil || len(prs) != 101 || prs[100].HeadRepo != "" || prs[0].HeadRepo != "acme/widgets" {
			t.Fatalf("got %d pull requests, err = %v", len(prs), err)
		}
		requests := s.Requests()
		if len(requests) != 2 || requests[0].Query.Get("state") != "open" || requests[1].Query.Get("page") != "2" {
			t.Errorf("requests = %+v", requests)
		}
	})
}
