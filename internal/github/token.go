package github

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// ghTimeout bounds `gh auth token`, which reads a local file or keyring.
const ghTimeout = 15 * time.Second

// ErrNoToken is returned when no GitHub token can be found.
var ErrNoToken = errors.New("no GitHub token")

// ResolveToken finds the token the Daemon acts as: GH_TOKEN from the
// environment, else what ghAuthToken yields (GHAuthToken in the binary).
// Nothing is stored; the token is asked for each time the process starts.
func ResolveToken(lookupEnv func(string) (string, bool), ghAuthToken func() (string, error)) (string, error) {
	if token, _ := lookupEnv("GH_TOKEN"); strings.TrimSpace(token) != "" {
		return strings.TrimSpace(token), nil
	}
	token, err := ghAuthToken()
	if token = strings.TrimSpace(token); err == nil && token != "" {
		return token, nil
	}
	reason := "it printed nothing"
	if err != nil {
		reason = err.Error()
	}
	return "", fmt.Errorf("%w: GH_TOKEN is not set and `gh auth token` gave none (%s)", ErrNoToken, reason)
}

// GHAuthToken asks the gh CLI for the token of its logged-in account. It is
// the only gh process the Daemon runs.
func GHAuthToken() (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), ghTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "gh", "auth", "token").Output()
	if err != nil {
		return "", fmt.Errorf("gh auth token: %w", err)
	}
	return string(out), nil
}
