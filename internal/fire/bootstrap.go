package fire

import (
	"context"
	_ "embed"
	"fmt"
	"sort"
	"strings"

	"github.com/benjr70/auto-agent/internal/config"
	"github.com/benjr70/auto-agent/internal/github"
	"github.com/benjr70/auto-agent/internal/journal"
	"github.com/benjr70/auto-agent/internal/pick"
	"github.com/benjr70/auto-agent/internal/state"
)

// BootstrapMarker is the one identity of the Bootstrap issue. It is matched
// in the body, so the title is a human's to change (ADR 0007).
const BootstrapMarker = "<!-- auto-agent:bootstrap -->"

// BootstrapTitle is the title the Bootstrap issue is opened with.
const BootstrapTitle = "Write the Environment provider for the hermetic verification tier"

// FindBootstrap is the open Bootstrap issue's number among the open AFK
// issues, or 0 when there is none. The lowest number wins, so two of them (a
// human opened one by hand) never flip between Fires. An error is not "none
// open": the lookup fails closed.
func FindBootstrap(ctx context.Context, gh *github.Client, repo config.Repo) (int, error) {
	open, err := gh.OpenIssues(ctx, repo.Owner, repo.Name, state.LabelAFK)
	if err != nil {
		return 0, err
	}
	lowest := 0
	for _, issue := range open {
		if strings.Contains(issue.Body, BootstrapMarker) && (lowest == 0 || issue.Number < lowest) {
			lowest = issue.Number
		}
	}
	return lowest, nil
}

// EnsureBootstrap makes sure a Target Project in the Bootstrap state has the
// one AFK ticket that asks the Daemon to write its Environment provider: it
// reuses the open one, or opens it and puts it on the pick signal. It says
// what it did, as the Fire record has it: "issue #N reused", "issue #N
// created", or under dryRun "would reuse #N" and "would open", writing
// nothing. Out of the Bootstrap state it asks GitHub nothing and says
// nothing. A ticket opened but not put on the pick signal is said, with the
// error.
func EnsureBootstrap(ctx context.Context, gh *github.Client, harness config.Harness, dryRun bool) (string, error) {
	if harness.Verification.Hermetic != nil {
		return "", nil
	}
	repo := harness.Repo
	number, err := FindBootstrap(ctx, gh, repo)
	if err != nil {
		return "", fmt.Errorf("the open %s issues could not be listed, so no second Bootstrap issue is opened: %w", state.LabelAFK, err)
	}
	switch {
	case number != 0 && dryRun:
		return fmt.Sprintf("would reuse #%d", number), nil
	case number != 0:
		return fmt.Sprintf("issue #%d reused", number), nil
	case dryRun:
		return "would open", nil
	}
	// AFK goes on as it is opened, so the next Fire finds it whatever the
	// pick signal's write does.
	issue, err := gh.CreateIssue(ctx, repo.Owner, repo.Name, github.NewIssue{
		Title: BootstrapTitle, Body: BootstrapBody(harness), Labels: []string{state.LabelAFK},
	})
	if err != nil {
		return "", fmt.Errorf("the Bootstrap issue could not be opened: %w", err)
	}
	line := fmt.Sprintf("issue #%d created", issue.Number)
	// Under a Project pick a ticket that is not on the board is never
	// picked, so the one ticket that asks for the provider would sit there.
	if _, err := pick.Add(ctx, gh, harness, issue.Number, ""); err != nil {
		return line, fmt.Errorf("the Bootstrap issue #%d is open but could not be put on the pick signal; publish it by hand: %w", issue.Number, err)
	}
	return line, nil
}

// BootstrapOwed is whether a Fire that derived kind ensures the Bootstrap
// issue: every Fire does but a Parked one and one that finds another in
// flight, which do nothing.
func BootstrapOwed(kind state.Kind) bool {
	return kind != state.KindParked && kind != state.KindInFlight
}

// bootstrap is EnsureBootstrap as a Fire runs it: what it did goes in the
// record, and what failed is a warning, never the Fire's error.
func (f *Fire) bootstrap(ctx context.Context, record *journal.Record, harness config.Harness) {
	harness.Repo = f.Repo
	line, err := EnsureBootstrap(ctx, f.GitHub, harness, false)
	record.Bootstrap = line
	if err != nil {
		record.Warnings = append(record.Warnings, err.Error())
	}
}

// bootstrapBody is the Bootstrap issue's body, the Surfaces it names left
// as {{surfaces}}.
//
//go:embed bootstrap.md
var bootstrapBody string

// BootstrapBody is the Bootstrap issue the Daemon reads: a Slice, as that is
// what the implementer knows how to work, naming the Surfaces the config
// declares.
func BootstrapBody(harness config.Harness) string {
	surfaces := "The config declares no Surface yet: declare the ones this project has."
	if len(harness.Surfaces) > 0 {
		names := make([]string, 0, len(harness.Surfaces))
		for name := range harness.Surfaces {
			names = append(names, name)
		}
		sort.Strings(names)
		lines := make([]string, len(names))
		for i, name := range names {
			s := harness.Surfaces[name]
			lines[i] = fmt.Sprintf("- `%s` (%s): its url_key is `%s`", name, s.Kind, s.URLKey)
		}
		surfaces = strings.Join(lines, "\n")
	}
	return strings.Replace(bootstrapBody, "{{surfaces}}", surfaces, 1)
}
