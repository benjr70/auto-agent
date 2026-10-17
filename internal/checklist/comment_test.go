package checklist

import (
	"strings"
	"testing"
)

func round() Round {
	return Round{
		N: 2, Max: 3,
		Dir: "/state/verify/pr-87/20261008T123114Z-r2",
		Results: []Result{
			{Item: Item{Section: Manual, Text: "the health route answers"}, Verdict: Pass, Classification: "local",
				Evidence: []string{"`GET /health` gave 200", "log: `listening on :8080`"}},
			{Item: Item{Section: Manual, Text: "the dashboard loads in prod <!-- post-deploy: e2e/prod.spec.ts -->", PostDeploy: "e2e/prod.spec.ts"},
				Verdict: Defer, Classification: "deployed-env", Deferral: "needs the deployed environment"},
			{Item: Item{Section: Human, Text: "the panel shows the reading"}, Verdict: Defer, Classification: "hardware",
				Deferral: "a human must read the physical panel"},
			{Item: Item{Section: Manual, Text: "the list shows every item"}, Verdict: Fail, Classification: "local",
				Evidence: []string{"the list showed 2 of 3 items"}},
		},
	}
}

// The round's one evidence comment, in today's shape.
func TestComment(t *testing.T) {
	r := round()
	got := Comment(r)
	cases := []struct {
		name string
		want []string
	}{
		{"comment: the heading names the round", []string{"### Manual verification — round 2/3\n\n"}},
		{"comment: the result line tallies every item", []string{"**Result: 1/4 PASS, 2 deferred, 1 FAIL.**"}},
		{"comment: a section per item with its verdict and classification", []string{
			"#### 1. the health route answers — PASS (local)\n\n- `GET /health` gave 200\n- log: `listening on :8080`\n",
			"#### 2. the dashboard loads in prod — DEFER (deployed-env)\n",
			"#### 3. the panel shows the reading — DEFER (hardware)\n",
			"#### 4. the list shows every item — FAIL (local)\n\n- the list showed 2 of 3 items\n",
		}},
		{"comment: a deferral says why", []string{
			"- **Deferred:** needs the deployed environment\n- **Post-deploy spec:** `e2e/prod.spec.ts`\n",
			"- **Deferred:** a human must read the physical panel\n",
		}},
		{"comment: the evidence directory is cited", []string{"\nEvidence directory: `/state/verify/pr-87/20261008T123114Z-r2`\n"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			for _, w := range c.want {
				if !strings.Contains(got, w) {
					t.Errorf("lacks %q:\n%s", w, got)
				}
			}
		})
	}
	t.Run("comment: no sandbox note when the sandbox is OK", func(t *testing.T) {
		if strings.Contains(got, "DEGRADED") {
			t.Errorf("says DEGRADED:\n%s", got)
		}
	})
	t.Run("comment: a DEGRADED sandbox is said", func(t *testing.T) {
		r.Sandbox = "no AppArmor profile grants user namespaces"
		if got := Comment(r); !strings.Contains(got, "**Sandbox DEGRADED:** no AppArmor profile grants user namespaces") {
			t.Errorf("no DEGRADED note:\n%s", got)
		}
	})
	t.Run("comment: a round with no number has a bare heading", func(t *testing.T) {
		r := round()
		r.N, r.Max = 0, 0
		if got := Comment(r); !strings.HasPrefix(got, "### Manual verification\n\n") {
			t.Errorf("heading:\n%s", got)
		}
	})
	t.Run("comment: the heading is what triage reads a round by", func(t *testing.T) {
		if first, _, _ := strings.Cut(got, "\n"); !strings.Contains(first, "Manual verification — ") || !strings.Contains(first, "round") {
			t.Errorf("heading %q", first)
		}
	})
}

func TestLine(t *testing.T) {
	r := round()
	if got := r.Line(); got != "manual-verify: 1/4 PASS, 2 deferred, 1 FAIL" {
		t.Errorf("got %q", got)
	}
	r.Sandbox = "off"
	if got := r.Line(); got != "manual-verify: 1/4 PASS, 2 deferred, 1 FAIL (sandbox DEGRADED)" {
		t.Errorf("got %q", got)
	}
	if got := (Round{}).Line(); got != "manual-verify: 0/0 PASS, 0 deferred, 0 FAIL" {
		t.Errorf("got %q", got)
	}
}

func TestPassed(t *testing.T) {
	got := round().Passed()
	if len(got) != 1 || got[0] != "the health route answers" {
		t.Errorf("got %v", got)
	}
	ticked := Tick("## Manual verification\n- [ ] the health route answers\n- [ ] the list shows every item\n", got)
	if ticked != "## Manual verification\n- [x] the health route answers\n- [ ] the list shows every item\n" {
		t.Errorf("ticked:\n%s", ticked)
	}
}

// The two rounds that give no verdict: an environment that did not boot, and
// a Target Project with no Environment provider at all.
func TestNoVerdictComments(t *testing.T) {
	t.Run("infra-error: the round's heading, no verdicts, the reason and the directory", func(t *testing.T) {
		got := InfraComment(1, 3, "up exited 3 (prerequisite missing): docker is not running", "/state/verify/pr-87/x-r1")
		for _, w := range []string{
			"### Manual verification — round 1/3: infrastructure error\n\n",
			"**No verdicts.** No item was exercised and no box is ticked: up exited 3 (prerequisite missing): docker is not running\n",
			"Evidence directory: `/state/verify/pr-87/x-r1`",
		} {
			if !strings.Contains(got, w) {
				t.Errorf("lacks %q:\n%s", w, got)
			}
		}
		if strings.Contains(got, "PASS") || strings.Contains(got, "FAIL") {
			t.Errorf("an infrastructure error invents a verdict:\n%s", got)
		}
	})
	t.Run("infra-error: the line says no item was verified", func(t *testing.T) {
		if got := InfraLine("the display does not answer"); got != "manual-verify: infra-error — the display does not answer (0 items verified)" {
			t.Errorf("line = %q", got)
		}
	})
	t.Run("bootstrap-state: one comment saying why, and that a human verifies it", func(t *testing.T) {
		got := BootstrapComment()
		for _, w := range []string{"### Manual verification — no round: Bootstrap state\n", "no hermetic tier", "AFK:verify-human"} {
			if !strings.Contains(got, w) {
				t.Errorf("lacks %q:\n%s", w, got)
			}
		}
	})
}
