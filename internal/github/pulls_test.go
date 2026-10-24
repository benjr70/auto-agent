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
		return map[string]any{"number": number, "title": "t", "state": "open", "head": head, "base": map[string]any{"ref": "trunk"},
			"html_url": fmt.Sprintf("https://github.com/acme/widgets/pull/%d", number)}
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
		if err != nil || len(prs) != 101 || prs[100].HeadRepo != "" || prs[0].HeadRepo != "acme/widgets" || prs[0].URL != "https://github.com/acme/widgets/pull/1" {
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
	// review-poster: "inline post anchors path/line/side/commit on the
	// configured repo"
	t.Run("inline post anchors path/line/side/commit on the configured repo", func(t *testing.T) {
		s := githubtest.New(t)
		s.Handle("POST", "/repos/acme/widgets/pulls/310/comments", githubtest.JSON(201, map[string]any{"id": 1}))
		err := newClient(s).CreateReviewComment(ctx, "acme", "widgets", 310, ReviewComment{Body: "the body", Path: "apps/a.ts", Line: 12, Side: "RIGHT", CommitID: "abc1234"})
		if err != nil {
			t.Fatal(err)
		}
		var sent map[string]any
		json.Unmarshal([]byte(s.Writes()[0].Body), &sent)
		if fmt.Sprint(sent) != fmt.Sprint(map[string]any{"body": "the body", "path": "apps/a.ts", "line": 12.0, "side": "RIGHT", "commit_id": "abc1234"}) {
			t.Errorf("sent %v", sent)
		}
	})
	// review-poster: "API failure exits non-zero", and the 422 the anchoring
	// fallback keys off
	t.Run("API failure exits non-zero, and a refused anchor is told apart", func(t *testing.T) {
		s := githubtest.New(t)
		s.Handle("POST", "/repos/acme/widgets/pulls/310/comments", githubtest.JSON(422, map[string]any{"message": "Validation Failed"}), githubtest.JSON(502, map[string]any{"message": "Bad Gateway"}))
		c := newClient(s)
		if err := c.CreateReviewComment(ctx, "acme", "widgets", 310, ReviewComment{}); err == nil || !IsUnprocessable(err) {
			t.Errorf("422: err = %v", err)
		}
		if err := c.CreateReviewComment(ctx, "acme", "widgets", 310, ReviewComment{}); err == nil || IsUnprocessable(err) {
			t.Errorf("502: err = %v", err)
		}
	})
	t.Run("the review comments are read page by page, with their author and line; an outdated one has none", func(t *testing.T) {
		s := githubtest.New(t)
		var full []map[string]any
		for i := 1; i <= 100; i++ {
			full = append(full, map[string]any{"id": i, "body": "b", "path": "a.go", "line": i, "side": "RIGHT", "user": map[string]any{"login": "machine-user"}})
		}
		s.Handle("GET", "/repos/acme/widgets/pulls/310/comments", githubtest.JSON(200, full), githubtest.JSON(200, []map[string]any{{"id": 101, "body": "b", "path": "a.go", "line": nil}}))
		comments, err := newClient(s).ReviewComments(ctx, "acme", "widgets", 310)
		if err != nil || len(comments) != 101 || comments[0].Author != "machine-user" || comments[0].Line != 1 || comments[100].Line != 0 {
			t.Fatalf("comments = %d, err = %v", len(comments), err)
		}
	})
	t.Run("a pull request read alone carries its body, and the body is edited in place", func(t *testing.T) {
		s := githubtest.New(t)
		read := pull(55, "feat/issue-20", "acme/widgets")
		read["body"] = "## Manual verification\n\n- [ ] it works\n"
		s.Handle("GET", "/repos/acme/widgets/pulls/55", githubtest.JSON(200, read))
		s.Handle("PATCH", "/repos/acme/widgets/pulls/55", githubtest.JSON(200, read))
		c := newClient(s)
		pr, err := c.PullRequest(ctx, "acme", "widgets", 55)
		if err != nil || pr.Body != "## Manual verification\n\n- [ ] it works\n" {
			t.Fatalf("pr = %+v, err = %v", pr, err)
		}
		if err := c.EditPullRequestBody(ctx, "acme", "widgets", 55, "ticked"); err != nil {
			t.Fatal(err)
		}
		if w := s.Writes(); len(w) != 1 || w[0].Method != "PATCH" || w[0].Body != `{"body":"ticked"}` {
			t.Errorf("writes = %+v", w)
		}
	})
}

func TestMerge(t *testing.T) {
	t.Run("a merge is a squash pinned to the head sha read, and returns the commit it landed as", func(t *testing.T) {
		s := githubtest.New(t)
		s.Handle("PUT", "/repos/acme/widgets/pulls/55/merge", githubtest.JSON(200, map[string]any{"sha": "m3rged", "merged": true}))
		sha, err := newClient(s).MergePullRequest(ctx, "acme", "widgets", 55, "abc123", "docs(research): the gate (#20) (#55)")
		if err != nil || sha != "m3rged" {
			t.Fatalf("sha = %q, err = %v", sha, err)
		}
		var sent map[string]string
		json.Unmarshal([]byte(s.Writes()[0].Body), &sent)
		want := map[string]string{"merge_method": "squash", "sha": "abc123", "commit_title": "docs(research): the gate (#20) (#55)"}
		if fmt.Sprint(sent) != fmt.Sprint(want) {
			t.Errorf("sent %v, want %v", sent, want)
		}
	})
	t.Run("a head that moved is not merged: GitHub's 409 is the error", func(t *testing.T) {
		s := githubtest.New(t)
		s.Handle("PUT", "/repos/acme/widgets/pulls/55/merge", githubtest.JSON(409, map[string]any{"message": "Head branch was modified. Review and try the merge again."}))
		if _, err := newClient(s).MergePullRequest(ctx, "acme", "widgets", 55, "abc123", "t"); err == nil || !strings.Contains(err.Error(), "Head branch was modified") {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("the merged pull requests are the closed ones GitHub says were merged, with the commit each landed as and when", func(t *testing.T) {
		s := githubtest.New(t)
		s.Handle("GET", "/repos/acme/widgets/pulls", githubtest.JSON(200, []map[string]any{
			{"number": 1, "state": "closed", "merged_at": "2026-10-01T00:00:00Z", "merge_commit_sha": "m1", "head": map[string]any{"ref": "research/a"}},
			{"number": 2, "state": "closed", "merged_at": nil, "merge_commit_sha": "t2", "head": map[string]any{"ref": "research/b"}},
		}))
		prs, err := newClient(s).MergedPullRequests(ctx, "acme", "widgets")
		if err != nil || len(prs) != 1 || prs[0].Number != 1 || !prs[0].Merged || prs[0].MergeSHA != "m1" || prs[0].MergedAt != "2026-10-01T00:00:00Z" || prs[0].HeadRef != "research/a" {
			t.Fatalf("prs = %+v, err = %v", prs, err)
		}
		q := s.Requests()[0].Query
		if q.Get("state") != "closed" || q.Get("sort") != "updated" || q.Get("direction") != "desc" {
			t.Errorf("query = %v", q)
		}
	})
	t.Run("a renamed file is both of its paths", func(t *testing.T) {
		s := githubtest.New(t)
		s.Handle("GET", "/repos/acme/widgets/pulls/55/files", githubtest.JSON(200, []map[string]any{
			{"filename": "docs/research/moved-spec.md", "previous_filename": "scripts/ralph/old-spec.md", "status": "renamed"},
			{"filename": "docs/research/a.md", "status": "added"},
		}))
		paths, err := newClient(s).Files(ctx, "acme", "widgets", 55)
		if err != nil || strings.Join(paths, ",") != "docs/research/moved-spec.md,scripts/ralph/old-spec.md,docs/research/a.md" {
			t.Errorf("paths = %v, err = %v", paths, err)
		}
	})
}
