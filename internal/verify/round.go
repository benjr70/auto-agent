package verify

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strconv"

	"github.com/benjr70/auto-agent/internal/config"
)

// UsageError is a round that cannot drive the provider at all: the PR head
// carries no readable Harness config, or its hermetic command is not an
// executable file. Nothing was booted.
type UsageError struct{ Detail string }

func (e *UsageError) Error() string { return e.Detail }

// InfraError is an environment that did not boot: up exited 3, or 4 twice,
// or printed no usable block. It is a round's infrastructure error, never a
// verdict on any checklist item. The environment is already torn down.
type InfraError struct {
	Detail string
	// Violation is set when the provider broke its contract.
	Violation bool
}

func (e *InfraError) Error() string { return "the environment did not boot: " + e.Detail }

// Round is a Verification round's environment, booted from the PR head.
type Round struct {
	// Harness is the config the PR head carries (ADR 0007): its Surfaces are
	// the ones the round drives.
	Harness config.Harness
	// Block is the provider's KEY=value block, in order.
	Block [][2]string

	provider Provider
	pr       string
	env      []string
}

// BootRound boots the environment for PR pr through the Environment provider
// the Harness config in head, the PR head's checkout, declares (ADR 0007): so
// the PR that adds a provider is verified by the provider it adds. It runs
// down --all before the first up, retries a failed boot once with a down between,
// and checks the block's grammar and every declared Surface's url_key.
//
// It returns ErrBootstrap when the head declares no hermetic tier, a
// *UsageError when the provider cannot be run, and an *InfraError when the
// environment did not boot; on every error the environment is down. On
// success the caller tears the Round down on every way out.
func BootRound(ctx context.Context, head string, repo config.Repo, pr int, env []string) (*Round, error) {
	// The provider runs in head, so a relative path would resolve twice.
	head, err := filepath.Abs(head)
	if err != nil {
		return nil, &UsageError{Detail: err.Error()}
	}
	harness, err := config.LoadHarness(head, "", repo)
	if err != nil {
		return nil, &UsageError{Detail: fmt.Sprintf("reading the PR head's Harness config: %v", err)}
	}
	provider, err := Resolve(head, harness.Verification.Hermetic)
	if errors.Is(err, ErrBootstrap) {
		return nil, err
	} else if err != nil {
		return nil, &UsageError{Detail: err.Error()}
	}
	r := &Round{Harness: harness, provider: provider, pr: strconv.Itoa(pr), env: env}
	if err := provider.downFirst(ctx, env); err != nil {
		return nil, &InfraError{Detail: err.Error(), Violation: true}
	}
	block, failed := provider.start(ctx, r.pr, harness.Surfaces, env, func() call { return r.down(ctx) })
	if failed != nil {
		r.Teardown(ctx)
		return nil, &InfraError{Detail: failed.Detail, Violation: failed.Violation}
	}
	r.Block = block
	return r, nil
}

// Teardown is the provider's down. It is idempotent, runs even when ctx is
// cancelled, and is safe on a nil Round: one that never booted.
func (r *Round) Teardown(ctx context.Context) error {
	if r == nil {
		return nil
	}
	if c := r.down(ctx); c.exit != 0 {
		return fmt.Errorf("down after up exited %d, want 0 (down is idempotent)", c.exit)
	}
	return nil
}

func (r *Round) down(ctx context.Context) call {
	return r.provider.run(context.WithoutCancel(ctx), r.env, "down", "--pr", r.pr)
}
