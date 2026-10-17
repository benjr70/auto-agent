package fire

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/benjr70/auto-agent/internal/checklist"
	"github.com/benjr70/auto-agent/internal/ci"
	"github.com/benjr70/auto-agent/internal/config"
	"github.com/benjr70/auto-agent/internal/github"
	"github.com/benjr70/auto-agent/internal/journal"
	"github.com/benjr70/auto-agent/internal/state"
	"github.com/benjr70/auto-agent/internal/step"
	"github.com/benjr70/auto-agent/internal/step/verifier"
	"github.com/benjr70/auto-agent/internal/verify"
)

// PhaseVerify: the Verification round of the PR.
const PhaseVerify = "verify"

// verifiable says CI leaves the PR to its Verification round: green, or no
// check at all, which has nothing red to say.
func verifiable(wait *journal.CI) bool { return wait.Result == ci.Green || wait.Result == ci.None }

// verifyRound runs one Verification round on the PR, its head checked out in
// the Workspace: the unticked items of its checklist go to a Verify Step on
// the Surfaces the head's Harness config declares (ADR 0007), in the
// environment booted from the head. Go ticks only what passed, puts the
// tour in the PR's description and posts the round's one evidence comment;
// when every item passed or was justifiably deferred, the ticket is
// AFK:done. The environment, the apps and the browser profiles are torn down
// on every way out. An environment that did not boot is an infrastructure
// error, with no verdict on any item; a head with no hermetic tier is the
// Bootstrap state, left to a human. finished is false when the Fire ends
// here, with err its error, if any.
func (f *Fire) verifyRound(ctx context.Context, record *journal.Record, t *ticket, pr int) (finished bool, err error) {
	owner, name := f.Repo.Owner, f.Repo.Name
	record.Phase = PhaseVerify
	v := &journal.Verify{Max: t.harness.Rounds.ManualVerify}
	record.Verify = v
	current, err := f.GitHub.PullRequest(ctx, owner, name, pr)
	if err != nil {
		return false, fmt.Errorf("reading PR #%d's checklist: %w", pr, err)
	}
	comments, err := f.GitHub.Comments(ctx, owner, name, pr)
	if err != nil {
		return false, fmt.Errorf("reading PR #%d's rounds: %w", pr, err)
	}
	last, _ := state.LastRound(comments, f.Login)
	v.Round = last.N + 1
	post := func(body string) error {
		if err := f.GitHub.CreateComment(ctx, owner, name, pr, body); err != nil {
			return fmt.Errorf("posting PR #%d's verification round: %w", pr, err)
		}
		return nil
	}
	infra := func(detail string) (bool, error) {
		v.Infra, v.Line = detail, checklist.InfraLine(detail)
		if err := post(checklist.InfraComment(v.Round, v.Max, detail, v.Dir)); err != nil {
			return false, err
		}
		return true, nil
	}

	dir := f.Workspace.Dir()
	head, err := config.LoadHarness(dir, "", f.Repo)
	if err != nil {
		return infra(fmt.Sprintf("reading the PR head's Harness config: %v", err))
	}
	if head.Verification.Hermetic == nil {
		return f.bootstrapRound(ctx, v, current)
	}
	items := checklist.Parse(current.Body)
	paths, err := f.Workspace.ChangedPaths(ctx)
	if err != nil {
		record.Warnings = append(record.Warnings, fmt.Sprintf("PR #%d's changed paths could not be read, so no screenshot tour is asked for: %v", pr, err))
	}
	tour := verify.Tour(paths, head.Surfaces)
	if len(items) == 0 && len(tour) == 0 {
		// Nothing to exercise and nothing to show: the round is its tally.
		r := checklist.Round{N: v.Round, Max: v.Max}
		v.Line, v.Done = r.Line(), true
		if err := post(checklist.Comment(r)); err != nil {
			return false, err
		}
		return true, f.done(ctx, t)
	}

	// Asked before the boot: a Fire told to wait boots nothing.
	verdict, ok, err := f.gate(ctx, record, verifier.Name)
	if !ok {
		return false, err
	}
	if v.Dir, err = checklist.Dir(f.Journal.StateDir, pr, v.Round, f.Now()); err != nil {
		return false, fmt.Errorf("making the round's evidence directory: %w", err)
	}
	env, err := f.providerEnv()
	if err != nil {
		return false, err
	}
	round, err := verify.BootRound(ctx, dir, f.Repo, pr, env)
	if errors.Is(err, verify.ErrBootstrap) {
		return f.bootstrapRound(ctx, v, current)
	} else if err != nil {
		return infra(err.Error())
	}
	defer func() {
		if err := round.Teardown(ctx); err != nil {
			record.Warnings = append(record.Warnings, "the Environment provider: "+err.Error())
		}
	}()
	launcher := verify.Launcher{Host: f.surfaceHost(), Dir: dir, RunDir: filepath.Join(v.Dir, "run")}
	session, err := launcher.Launch(ctx, round.Harness.Surfaces, apps(round.Harness.Surfaces), round.Block)
	if err != nil {
		return infra(err.Error())
	}
	defer func() {
		if err := session.Close(); err != nil {
			record.Warnings = append(record.Warnings, "tearing down the round's Surfaces: "+err.Error())
		}
	}()

	input := verifier.Input{
		PR: pr, Branch: t.branch, Issue: t.stepIssue(), Items: verifier.Items(items), Block: blockLines(round.Block),
		Surfaces: stepSurfaces(round.Harness.Surfaces, round.Block, tour, session.MCP), EvidenceDir: v.Dir,
		Sandbox: session.Sandbox, Runbook: runbook(round.Harness.Prose.VerifierRunbook),
	}
	call := step.Call{Dir: dir, Model: f.model(*verdict), Input: input, MCP: session.MCP}
	result, run := callStep(ctx, f, record, verifier.Spec(), call, *verdict, func(r verifier.Result) error { return r.Check(items) })
	// The round changes nothing in the checkout: whatever it left is nobody's.
	if err := f.Workspace.Discard(ctx); err != nil {
		return false, fmt.Errorf("discarding what the round left in the checkout: %w", err)
	}
	if stop, err := f.settle(ctx, record, t, verifier.Name, run, ""); stop {
		return false, err
	}

	r := checklist.Round{N: v.Round, Max: v.Max, Results: result.Results(items), Dir: v.Dir, Sandbox: session.Sandbox}
	shots := checklist.Tour(result.TourLines(), v.Dir)
	if body := checklist.Inject(checklist.Tick(current.Body, r.Passed()), shots); body != current.Body {
		if err := f.GitHub.EditPullRequestBody(ctx, owner, name, pr, body); err != nil {
			return false, fmt.Errorf("ticking PR #%d's checklist: %w", pr, err)
		}
	}
	v.Shots, v.Line = len(shots), r.Line()
	if err := post(checklist.Comment(r)); err != nil {
		return false, err
	}
	if slices.ContainsFunc(r.Results, func(res checklist.Result) bool { return res.Verdict == checklist.Fail }) {
		return true, nil
	}
	v.Done = true
	return true, f.done(ctx, t)
}

// bootstrapRound is the round of a PR whose head declares no hermetic tier:
// no round, a human verifies it, and one comment says why.
func (f *Fire) bootstrapRound(ctx context.Context, v *journal.Verify, current github.PullRequest) (bool, error) {
	owner, name, pr := f.Repo.Owner, f.Repo.Name, current.Number
	v.Bootstrap, v.Round, v.Line = true, 0, "manual-verify: SKIPPED — Bootstrap state, "+state.LabelVerifyHuman+" applied"
	// The Fire that opened the PR may have applied it already.
	if !slices.Contains(current.Labels, state.LabelVerifyHuman) {
		if err := f.GitHub.AddLabels(ctx, owner, name, pr, state.LabelVerifyHuman); err != nil {
			return false, fmt.Errorf("handing PR #%d to a human verifier: %w", pr, err)
		}
	}
	if err := f.GitHub.CreateComment(ctx, owner, name, pr, checklist.BootstrapComment()); err != nil {
		return false, fmt.Errorf("saying why PR #%d has no verification round: %w", pr, err)
	}
	return true, nil
}

// done marks the ticket AFK:done: its PR's every item passed or was
// justifiably deferred. The Single-flight lock comes off as the Fire ends.
func (f *Fire) done(ctx context.Context, t *ticket) error {
	if err := f.GitHub.AddLabels(ctx, f.Repo.Owner, f.Repo.Name, t.issue.Number, state.LabelDone); err != nil {
		return fmt.Errorf("marking #%d %s: %w", t.issue.Number, state.LabelDone, err)
	}
	return nil
}

// providerEnv is the environment the Environment provider runs in: the
// Daemon's, with no GitHub credential, as it runs the Target Project's code.
func (f *Fire) providerEnv() ([]string, error) {
	empty := filepath.Join(f.Journal.StateDir, "provider-gh")
	if err := os.MkdirAll(empty, 0o700); err != nil {
		return nil, err
	}
	return step.CredentialFree(os.Environ(), empty), nil
}

// surfaceHost is the Host the round's Surfaces run on: the Daemon's own
// environment, unless the Fire was given one.
func (f *Fire) surfaceHost() verify.Host {
	host := f.SurfaceHost
	if host.Env == nil {
		host.Env = os.Environ()
	}
	return host
}

// apps are the electron Surfaces, whose apps Go launches for the round: an
// item may need any of them.
func apps(surfaces map[string]config.Surface) []string {
	var names []string
	for _, name := range slices.Sorted(maps.Keys(surfaces)) {
		if surfaces[name].Kind == verify.KindElectron {
			names = append(names, name)
		}
	}
	return names
}

// serverOf is the MCP server the launchers name for a UI Surface.
func serverOf(surface string) string { return "surface-" + surface }

// stepSurfaces are the declared Surfaces as the Verify Step is told them:
// each one's URL from the block, its viewport, the MCP server that drives
// it, and whether it is on the tour.
func stepSurfaces(surfaces map[string]config.Surface, block [][2]string, tour []string, servers map[string]step.MCPServer) []verifier.Surface {
	values := map[string]string{}
	for _, kv := range block {
		values[kv[0]] = kv[1]
	}
	told := []verifier.Surface{}
	for _, name := range slices.Sorted(maps.Keys(surfaces)) {
		s := surfaces[name]
		surface := verifier.Surface{Name: name, Kind: s.Kind, Tour: slices.Contains(tour, name)}
		if s.URLKey != "" {
			surface.URL = values[s.URLKey]
		}
		if _, ok := servers[serverOf(name)]; ok {
			surface.Server = serverOf(name)
			surface.Viewport, _ = verify.Viewport(surfaces, name)
		}
		told = append(told, surface)
	}
	return told
}

// blockLines is the provider's block as its KEY=value lines.
func blockLines(block [][2]string) []string {
	lines := make([]string, len(block))
	for i, kv := range block {
		lines[i] = kv[0] + "=" + kv[1]
	}
	return lines
}

// runbook is the verifier runbook's text; empty when there is none.
func runbook(path string) string {
	if path == "" {
		return ""
	}
	text, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(text))
}
