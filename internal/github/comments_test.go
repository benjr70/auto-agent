package github

import (
	"fmt"
	"testing"

	"github.com/benjr70/auto-agent/internal/github/githubtest"
)

func TestComments(t *testing.T) {
	t.Run("the comments of an issue are read page by page, with who wrote each and when", func(t *testing.T) {
		s := githubtest.New(t)
		var full []map[string]any
		for i := 1; i <= 100; i++ {
			full = append(full, map[string]any{"id": i, "body": fmt.Sprintf("c%d", i), "user": map[string]any{"login": "someone"}, "created_at": "2026-10-01T00:00:00Z"})
		}
		s.Handle("GET", "/repos/acme/widgets/issues/7/comments", githubtest.JSON(200, full),
			githubtest.JSON(200, []map[string]any{{"id": 5000000000, "body": "last", "user": nil}}))
		comments, err := newClient(s).Comments(ctx, "acme", "widgets", 7)
		if err != nil || len(comments) != 101 || comments[0] != (Comment{ID: 1, Author: "someone", Body: "c1", CreatedAt: "2026-10-01T00:00:00Z"}) || comments[100] != (Comment{ID: 5000000000, Body: "last"}) {
			t.Fatalf("got %d comments, err = %v", len(comments), err)
		}
		if requests := s.Requests(); len(requests) != 2 || requests[0].Query.Get("per_page") != "100" || requests[1].Query.Get("page") != "2" {
			t.Errorf("requests = %+v", requests)
		}
	})
	t.Run("updating a comment patches its body by id", func(t *testing.T) {
		s := githubtest.New(t)
		s.Handle("PATCH", "/repos/acme/widgets/issues/comments/42", githubtest.Text(200, `{}`))
		if err := newClient(s).UpdateComment(ctx, "acme", "widgets", 42, "new"); err != nil {
			t.Fatal(err)
		}
		if writes := s.Writes(); len(writes) != 1 || writes[0].Body != `{"body":"new"}` {
			t.Errorf("writes = %+v", writes)
		}
	})
}
