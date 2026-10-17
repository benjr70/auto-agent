package github

import (
	"reflect"
	"testing"

	"github.com/benjr70/auto-agent/internal/github/githubtest"
)

func TestLabels(t *testing.T) {
	t.Run("the repository's label names are read, every page", func(t *testing.T) {
		s := githubtest.New(t)
		page := make([]map[string]any, labelsPage)
		for i := range page {
			page[i] = map[string]any{"name": "l", "color": "000000"}
		}
		page[0]["name"] = "AFK"
		s.Handle("GET", "/repos/acme/widgets/labels", githubtest.JSON(200, page), githubtest.JSON(200, []map[string]any{{"name": "spec"}}))
		got, err := newClient(s).Labels(ctx, "acme", "widgets")
		if err != nil || len(got) != labelsPage+1 || got[0] != "AFK" || got[labelsPage] != "spec" {
			t.Fatalf("Labels = %d names, %v", len(got), err)
		}
		if q := s.Requests()[1].Query; q.Get("page") != "2" || q.Get("per_page") != "100" {
			t.Errorf("second page query = %v", q)
		}
	})
	t.Run("a label is created with its colour and description", func(t *testing.T) {
		s := githubtest.New(t)
		s.Handle("POST", "/repos/acme/widgets/labels", githubtest.JSON(201, map[string]any{"name": "AFK"}))
		if err := newClient(s).CreateLabel(ctx, "acme", "widgets", Label{Name: "AFK", Color: "1D76DB", Description: "d"}); err != nil {
			t.Fatal(err)
		}
		writes := s.Writes()
		if len(writes) != 1 || !reflect.DeepEqual(writes[0].Body, `{"name":"AFK","color":"1D76DB","description":"d"}`) {
			t.Errorf("writes = %+v", writes)
		}
	})
}
