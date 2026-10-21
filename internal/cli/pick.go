package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/benjr70/auto-agent/internal/config"
	"github.com/benjr70/auto-agent/internal/daemon"
	"github.com/benjr70/auto-agent/internal/pick"
)

// pickCmd puts a ticket on the pick signal or takes it off, and says what
// landed; it exits non-zero, saying what failed, when it did not land.
func pickCmd(args []string, env Env) int {
	flags := flag.NewFlagSet("pick", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	priority := flags.String("priority", "", "")
	repoFlag := flags.String("repo", "", "")
	var positional []string
	for rest := args; ; {
		err := flags.Parse(rest)
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprint(env.Stdout, usage)
			return exitOK
		}
		if err != nil {
			return usageError(env.Stderr, "pick: "+err.Error())
		}
		if flags.NArg() == 0 {
			break
		}
		positional = append(positional, flags.Arg(0))
		rest = flags.Args()[1:]
	}
	if len(positional) > 0 && positional[0] == "labels" {
		return pickLabels(positional[1:], *priority, *repoFlag, env)
	}
	if len(positional) < 2 || len(positional) > 3 {
		return usageError(env.Stderr, "pick takes add|remove, an issue number and at most one target dir")
	}
	verb := positional[0]
	if verb != "add" && verb != "remove" {
		return usageError(env.Stderr, fmt.Sprintf("pick: unknown verb %q: add, remove or labels", verb))
	}
	if verb == "remove" && *priority != "" {
		return usageError(env.Stderr, "pick: remove takes no --priority")
	}
	issue, err := strconv.Atoi(positional[1])
	if err != nil || issue <= 0 {
		return usageError(env.Stderr, fmt.Sprintf("pick: %q is not an issue number", positional[1]))
	}
	targetDir := "."
	if len(positional) == 3 {
		targetDir = positional[2]
	}
	fail := func(err error) int {
		fmt.Fprintf(env.Stderr, "auto-agent: pick: %s #%d: %v\n", verb, issue, err)
		var refused *pick.Error
		if errors.As(err, &refused) && refused.Reason == pick.ReasonUnknownPriority {
			return exitUsage
		}
		return exitFailed
	}

	ctx := context.Background()
	s, err := openSession(ctx, env, targetDir, *repoFlag)
	if err != nil {
		return fail(err)
	}
	harness, err := config.LoadHarness(targetDir, "", s.repo)
	if err != nil {
		return fail(err)
	}
	var result pick.Result
	if verb == "add" {
		result, err = pick.Add(ctx, s.client, harness, issue, *priority)
	} else {
		result, err = pick.Remove(ctx, s.client, harness, issue)
	}
	if len(result.LabelsCreated) > 0 {
		fmt.Fprintf(env.Stdout, "pick: created the labels %s\n", strings.Join(result.LabelsCreated, ", "))
	}
	if err != nil {
		return fail(err)
	}
	fmt.Fprintln(env.Stdout, pickLine(verb, result, harness.Pick))
	return exitOK
}

// pickLabels ensures the harness labels alone: what a skill runs before it
// creates a ticket labelled with one, such as a Spec, that never goes on the
// pick signal.
func pickLabels(positional []string, priority, repoSlug string, env Env) int {
	if len(positional) > 1 || priority != "" {
		return usageError(env.Stderr, "pick labels takes no issue, no --priority and at most one target dir")
	}
	targetDir := "."
	if len(positional) == 1 {
		targetDir = positional[0]
	}
	ctx := context.Background()
	s, err := openSession(ctx, env, targetDir, repoSlug)
	var created []string
	present := 0
	if err == nil {
		created, present, err = daemon.EnsureLabels(ctx, s.client, s.repo.Owner, s.repo.Name)
	}
	if len(created) > 0 {
		fmt.Fprintf(env.Stdout, "pick: created the labels %s\n", strings.Join(created, ", "))
	}
	if err != nil {
		fmt.Fprintf(env.Stderr, "auto-agent: pick: labels: %v\n", err)
		return exitFailed
	}
	fmt.Fprintf(env.Stdout, "pick: labels created %d, present %d\n", len(created), present)
	return exitOK
}

// pickLine says what landed.
func pickLine(verb string, result pick.Result, signal config.Pick) string {
	if verb == "add" {
		line := fmt.Sprintf("pick: #%d is on the pick signal: %s", result.Issue, pick.Label)
		if signal.Shape == config.PickProject {
			line += fmt.Sprintf(", on project %d with %s %s", signal.Project.Number, signal.Project.PriorityField, result.Priority)
		}
		return line
	}
	line := fmt.Sprintf("pick: #%d is off the pick signal: no %s", result.Issue, pick.Label)
	if signal.Shape == config.PickProject {
		if result.OnBoard {
			line += fmt.Sprintf(", taken off project %d", signal.Project.Number)
		} else {
			line += fmt.Sprintf(", not on project %d", signal.Project.Number)
		}
	}
	return line
}
