package daemon

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/benjr70/auto-agent/internal/github"
	"github.com/benjr70/auto-agent/internal/state"
)

// Labeler is what EnsureLabels needs of GitHub (github.Client).
type Labeler interface {
	Labels(ctx context.Context, owner, repo string) ([]string, error)
	CreateLabel(ctx context.Context, owner, repo string, label github.Label) error
}

// EnsureLabels creates every label of the harness's vocabulary that the
// repository lacks. One it has is left as it is: its colour and description
// may be curated. A create that fails does not stop the others; the error
// names every one that failed.
func EnsureLabels(ctx context.Context, gh Labeler, owner, repo string) (created []string, present int, err error) {
	existing, err := gh.Labels(ctx, owner, repo)
	if err != nil {
		return nil, 0, fmt.Errorf("reading the labels of %s/%s: %w", owner, repo, err)
	}
	created = []string{}
	var failed []error
	for _, label := range state.HarnessLabels {
		if slices.Contains(existing, label.Name) {
			present++
			continue
		}
		if err := gh.CreateLabel(ctx, owner, repo, label); err != nil {
			failed = append(failed, fmt.Errorf("creating the label %s: %w", label.Name, err))
			continue
		}
		created = append(created, label.Name)
	}
	return created, present, errors.Join(failed...)
}
