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
	t.Run("a pull request read alone says whether it merges; a listing does not, and carries its author, labels and age", func(t *testing.T) {
		s := githubtest.New(t)
		listed := pull(7, "feat/issue-7", "acme/widgets")
		listed["user"] = map[string]any{"login": "machine-user"}
		listed["labels"] = []map[string]any{{"name": "AFK:revise"}}
		listed["created_at"] = "2026-10-01T00:00:00Z"
		s.Handle("GET", "/repos/acme/widgets/pulls", githubtest.JSON(200, []map[string]any{listed}))
		prs, err := newClient(s).OpenPullRequests(ctx, "acme", "widgets")
		if err != nil || len(prs) != 1 || prs[0].Author != "machine-user" || fmt.Sprint(prs[0].Labels) != "[AFK:revise]" ||
			prs[0].CreatedAt != "2026-10-01T00:00:00Z" || prs[0].Mergeable != MergeableUnknown {
			t.Fatalf("prs = %+v, err = %v", prs, err)
		}
		for value, want := range map[any]Mergeability{true: Mergeable, false: Conflicting, nil: MergeableUnknown} {
			one := pull(7, "feat/issue-7", "acme/widgets")
			one["mergeable"] = value
			s.Handle("GET", "/repos/acme/widgets/pulls/7", githubtest.JSON(200, one))
			if pr, err := newClient(s).PullRequest(ctx, "acme", "widgets", 7); err != nil || pr.Mergeable != want {
				t.Errorf("mergeable %v: got %q, err = %v", value, pr.Mergeable, err)
			}
		}
	})
	t.Run("the files a pull request changes are read page by page", func(t *testing.T) {
		s := githubtest.New(t)
		var full []map[string]string
		for i := 1; i <= 100; i++ {
			full = append(full, map[string]string{"filename": fmt.Sprintf("docs/research/%d.md", i)})
		}
		s.Handle("GET", "/repos/acme/widgets/pulls/7/files", githubtest.JSON(200, full), githubtest.JSON(200, []map[string]string{{"filename": "main.go"}}))
		files, err := newClient(s).Files(ctx, "acme", "widgets", 7)
		if err != nil || len(files) != 101 || files[0] != "docs/research/1.md" || files[100] != "main.go" {
			t.Fatalf("files = %v, err = %v", files, err)
		}
	})
}
