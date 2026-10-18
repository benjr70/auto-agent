package daemon

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/benjr70/auto-agent/internal/github"
	"github.com/benjr70/auto-agent/internal/state"
)

// Labeler is what EnsureLabels needs of GitHub (github.Client).
type Labeler interface {
	Labels(ctx context.Context, owner, repo string) ([]string, error)
	CreateLabel(ctx context.Context, owner, repo string, label github.Label) error
}

// EnsureLabels creates every label of the harness's vocabulary that the
// repository lacks, comparing names in any case as GitHub does. One it has
// is left as it is: its colour and description may be curated. A create
// that fails does not stop the others; the error names every one that
// failed. A create GitHub refuses because the label exists is no failure.
func EnsureLabels(ctx context.Context, gh Labeler, owner, repo string) (created []string, present int, err error) {
	existing, err := gh.Labels(ctx, owner, repo)
	if err != nil {
		return nil, 0, fmt.Errorf("reading the labels of %s/%s: %w", owner, repo, err)
	}
	// GitHub's label names are case-insensitive: Spec is spec.
	have := map[string]bool{}
	for _, name := range existing {
		have[strings.ToLower(name)] = true
	}
	created = []string{}
	var failed []error
	for _, label := range state.HarnessLabels {
		if have[strings.ToLower(label.Name)] {
			present++
			continue
		}
		err := gh.CreateLabel(ctx, owner, repo, label)
		if github.IsAlreadyExists(err) {
			// Created since the list was read: it is there, and left alone.
			present++
			continue
		}
		if err != nil {
			failed = append(failed, fmt.Errorf("creating the label %s: %w", label.Name, err))
			continue
		}
		created = append(created, label.Name)
	}
	return created, present, errors.Join(failed...)
}
