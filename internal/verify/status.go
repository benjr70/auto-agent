package verify

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/benjr70/auto-agent/internal/config"
)

// The exit codes of the deployed command's status.
const (
	statusHealthy      = 0
	statusUnhealthy    = 1
	statusPrerequisite = 3
)

// StatusBlock reads the live environment's KEY=value block from the deployed
// tier's command, run in dir, the default branch's checkout: `status`, and
// nothing else. The live environment is shared and was booted by nobody
// here, so the Deployed tier never calls up or down (ADR 0003). The block
// must carry the url_key of every declared Surface, as up's must.
//
// Every failure is an *InfraError, a round's infrastructure error and never
// a verdict on any item: status exiting 1 (unhealthy) or 3 (prerequisite
// missing) says the environment is not reachable; a command that is not
// executable, an exit outside the contract, a block that is not KEY=value,
// or a missing url_key is a command that broke its contract, a Violation.
func StatusBlock(ctx context.Context, dir string, deployed *config.Deployed, surfaces map[string]config.Surface, env []string) ([][2]string, error) {
	if deployed == nil {
		return nil, &InfraError{Detail: "the Harness config declares no deployed tier", Violation: true}
	}
	path := deployed.Command
	if !filepath.IsAbs(path) {
		path = filepath.Join(dir, path)
	}
	if info, err := os.Stat(path); err != nil || !info.Mode().IsRegular() || info.Mode()&0o111 == 0 {
		return nil, &InfraError{Detail: fmt.Sprintf("the deployed command %s is not an executable file under %s", deployed.Command, dir), Violation: true}
	}
	c := Provider{Path: path, Dir: dir}.run(ctx, env, "status")
	switch c.exit {
	case statusHealthy:
	case statusUnhealthy:
		return nil, &InfraError{Detail: "status exited 1 (the deployed environment is unhealthy): " + tail(c.stderr)}
	case statusPrerequisite:
		return nil, &InfraError{Detail: "status exited 3 (prerequisite missing): " + tail(c.stderr)}
	default:
		return nil, &InfraError{Detail: fmt.Sprintf("status exited %d, want 0 healthy, 1 unhealthy or 3 prerequisite missing: %s", c.exit, tail(c.stderr)), Violation: true}
	}
	// The contract's grammar, worded for up; here it was status that printed.
	block, err := Block(c.stdout)
	if err != nil {
		return nil, &InfraError{Detail: "status exited 0 but its stdout is not a KEY=value block (progress belongs on stderr): " + strings.Replace(err.Error(), "up printed", "status printed", 1), Violation: true}
	}
	if missing := missingURLKey(block, surfaces); missing != "" {
		return nil, &InfraError{Detail: strings.Replace(missing, "the up block", "the status block", 1), Violation: true}
	}
	return block, nil
}
