package step

import (
	"context"
	"os"
	"testing"
)

// TestLiveToyStep runs the toy Step against the real claude. CI never calls
// a model: this runs only by hand, with AUTO_AGENT_LIVE_CLAUDE naming the
// model to spend on (haiku is enough).
//
//	AUTO_AGENT_LIVE_CLAUDE=haiku go test ./internal/step -run TestLiveToyStep -v
func TestLiveToyStep(t *testing.T) {
	model := os.Getenv("AUTO_AGENT_LIVE_CLAUDE")
	if model == "" {
		t.Skip("set AUTO_AGENT_LIVE_CLAUDE to a model to run the toy Step against the real claude")
	}
	run := (&Claude{}).Run(context.Background(), toySpec(), Call{Dir: t.TempDir(), Model: model, Input: toyInput{Word: "hello"}})
	t.Logf("outcome %s (%s); session %s; model %s; tools %q; turns %d; cost $%.4f; %d rate-limit events; %d denials; %s",
		run.Outcome, run.Detail, run.SessionID, run.Model, run.Tools, run.NumTurns, run.CostUSD, len(run.RateLimits), len(run.PermissionDenials), run.Elapsed)
	result, err := Decode[toyResult](run)
	if err != nil {
		t.Fatal(err)
	}
	if result.Echo != "hello" {
		t.Errorf("echo = %q, want hello", result.Echo)
	}
}
