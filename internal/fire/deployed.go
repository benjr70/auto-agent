package fire

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/benjr70/auto-agent/internal/budget"
	"github.com/benjr70/auto-agent/internal/checklist"
	"github.com/benjr70/auto-agent/internal/config"
	"github.com/benjr70/auto-agent/internal/journal"
	"github.com/benjr70/auto-agent/internal/state"
	"github.com/benjr70/auto-agent/internal/step"
	"github.com/benjr70/auto-agent/internal/step/implement"
	"github.com/benjr70/auto-agent/internal/step/verifier"
	"github.com/benjr70/auto-agent/internal/verify"
)

// PhaseDeployed: a Deployed tier round on a merged Agent PR.
const PhaseDeployed = "deployed"

// deployedRound runs one Deployed tier round on a merged Agent PR, as
// plugin/skills/verify-deploy did: its deferred items, the unticked
// post-deploy tagged ones, go read-only to a Verify Step against the live
// environment that the deployed command's status resolves. Go calls status
// and nothing else: never up, never down, as nothing here booted the live
// environment and nothing tears it down. Go ticks what passed in the PR's
// body and posts the round's one comment; a failed item is reported, never
// fixed, as the PR is merged. A status that is not healthy, or a block that
// breaks the contract, is an infrastructure error: no item has a verdict,
// and the round still counts. No lock is taken: the PR is merged, and the
// round only ticks its boxes and posts one comment. The Harness config is
// the default branch's, as the PR's has merged into it.
func (f *Fire) deployedRound(ctx context.Context, record *journal.Record, harness config.Harness, d *state.Deployed) error {
	owner, name, pr := f.Repo.Owner, f.Repo.Name, d.PR
	record.PR = &pr
	record.Phase = PhaseDeployed
	v := &journal.Verify{Round: d.Round, Max: d.Max}
	record.Deployed = v
	// The body as it is now: a human may have ticked an item since the pick.
	current, err := f.GitHub.PullRequest(ctx, owner, name, pr)
	if err != nil {
		return fmt.Errorf("reading PR #%d's checklist: %w", pr, err)
	}
	items := checklist.Deferred(current.Body)
	if len(items) == 0 {
		v.Line, v.Done, record.Phase = "deployed-verify: 0/0 PASS, 0 deferred, 0 FAIL", true, PhaseDone
		return nil
	}
	post := func(body string) error {
		if err := f.GitHub.CreateComment(ctx, owner, name, pr, body); err != nil {
			return fmt.Errorf("posting PR #%d's Deployed tier round: %w", pr, err)
		}
		return nil
	}
	// A round's evidence directory is kept when its comment names it; one
	// the round left empty is not, and is gone when the round ends.
	named := false
	infra := func(detail string) error {
		v.Infra, v.Line = detail, checklist.DeployedInfraLine(detail)
		v.Dir = checklist.Prune(v.Dir)
		named = v.Dir != ""
		return post(checklist.DeployedInfraComment(v.Round, v.Max, detail, v.Dir))
	}

	// Asked before status: a Fire told to wait runs nothing.
	verdict, ok, err := f.gate(ctx, record, verifier.Name)
	if !ok {
		return err
	}
	if v.Dir, err = checklist.Dir(f.Journal.StateDir, pr, v.Round, f.Now()); err != nil {
		return fmt.Errorf("making the round's evidence directory: %w", err)
	}
	defer func() {
		if !named {
			v.Dir = checklist.Prune(v.Dir)
		}
	}()
	env, err := f.providerEnv()
	if err != nil {
		return err
	}
	dir := f.Workspace.Dir()
	block, err := verify.StatusBlock(ctx, dir, harness.Verification.Deployed, harness.Surfaces, env)
	if err != nil {
		var unreachable *verify.InfraError
		detail := err.Error()
		if errors.As(err, &unreachable) {
			detail = unreachable.Detail
		}
		if err := infra(detail); err != nil {
			return err
		}
		record.Phase = PhaseDone
		return nil
	}
	surfaces := liveSurfaces(harness.Surfaces)
	launcher := verify.Launcher{Host: f.surfaceHost(), Dir: dir, RunDir: filepath.Join(v.Dir, "run")}
	session, err := launcher.Launch(ctx, surfaces, nil, block)
	if err != nil {
		if err := infra(err.Error()); err != nil {
			return err
		}
		record.Phase = PhaseDone
		return nil
	}
	defer func() {
		if err := session.Close(); err != nil {
			record.Warnings = append(record.Warnings, "tearing down the round's Surfaces: "+err.Error())
		}
	}()

	issue := implement.Issue{Number: d.Issue, Title: d.Title}
	if ticket, err := f.GitHub.Issue(ctx, owner, name, d.Issue); err != nil {
		record.Warnings = append(record.Warnings, fmt.Sprintf("ticket #%d could not be read, so the Verify Step is told only PR #%d's title: %v", d.Issue, pr, err))
	} else {
		issue.Title, issue.Body = ticket.Title, ticket.Body
	}
	input := verifier.Input{
		PR: pr, Branch: d.Branch, Issue: issue, Items: verifier.Items(items), Block: blockLines(block),
		Surfaces: stepSurfaces(surfaces, block, nil, session.MCP), EvidenceDir: v.Dir, Sandbox: session.Sandbox,
		Runbook: runbook(harness.Prose.VerifierRunbook), Deployed: true, Checks: runbook(harness.Prose.DeployedChecks),
	}
	call := step.Call{Dir: dir, Model: f.model(*verdict), Input: input, MCP: session.MCP}
	result, run := callStep(ctx, f, record, verifier.Spec(), call, *verdict, func(r verifier.Result) error { return r.Check(items) })
	// The round changes nothing in the checkout: whatever it left is nobody's.
	if err := f.Workspace.Discard(ctx); err != nil {
		return fmt.Errorf("discarding what the round left in the checkout: %w", err)
	}
	switch run.Outcome {
	case step.OK:
	case step.Exhausted:
		// Not the PR's fault, and not a round: the next Fire runs it.
		record.ResetAt = budget.ReportOf(run, f.Now()).ResetAt
		return nil
	case step.AuthDead:
		return f.parkOnDead(ctx, record, fmt.Errorf("the %s Step of Fire %s ended %s: %s", verifier.Name, record.FireID, run.Outcome, run.Detail))
	default:
		failed := fmt.Errorf("the %s Step ended %s: %s", verifier.Name, run.Outcome, run.Detail)
		if stopping(ctx) {
			return failed
		}
		// It counts as a round, so a Step that keeps failing does not hold
		// the Lane for ever.
		if err := infra(failed.Error()); err != nil {
			return err
		}
		return failed
	}

	r := checklist.Round{N: v.Round, Max: v.Max, Results: result.Results(items), Dir: v.Dir, Sandbox: session.Sandbox}
	if body := checklist.Tick(current.Body, r.Passed()); body != current.Body {
		if err := f.GitHub.EditPullRequestBody(ctx, owner, name, pr, body); err != nil {
			return fmt.Errorf("ticking PR #%d's checklist: %w", pr, err)
		}
	}
	v.Line = checklist.DeployedLine(r)
	named = true
	if err := post(checklist.DeployedComment(r)); err != nil {
		return err
	}
	v.Done = len(r.Passed()) == len(r.Results)
	v.Exhausted = !v.Done && v.Round >= v.Max
	record.Phase = PhaseDone
	return nil
}

// liveSurfaces are the declared Surfaces a round on the live environment
// drives: every one but an electron app's, which would be launched here, on
// the Host, and is not the live environment.
func liveSurfaces(surfaces map[string]config.Surface) map[string]config.Surface {
	live := map[string]config.Surface{}
	for name, s := range surfaces {
		if s.Kind != verify.KindElectron {
			live[name] = s
		}
	}
	return live
}
