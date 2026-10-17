package state

import (
	"regexp"
	"strconv"
	"strings"

	"github.com/benjr70/auto-agent/internal/github"
)

// Round is a Verification round's verdict, read back from the comment the
// round posted on the PR: the Daemon keeps no store of its own (ADR 0012).
type Round struct {
	// N and Max are the round and the cap; 0 when the comment names none.
	N, Max int
	// The tally of the items it acted on.
	Pass, Total, Deferred, Fail int
	// Infra: the environment did not boot, and no item has a verdict.
	Infra bool
	// Bootstrap: no round ran, as the head declares no hermetic tier.
	Bootstrap bool
}

// Done says the round's every item passed or was justifiably deferred.
func (r Round) Done() bool { return !r.Infra && !r.Bootstrap && r.Fail == 0 }

var (
	roundNumber    = regexp.MustCompile(`Manual verification — round (\d+)/(\d+)`)
	roundTally     = regexp.MustCompile(`\*\*Result: (\d+)/(\d+) PASS, (\d+) deferred, (\d+) FAIL\.\*\*`)
	roundInfra     = regexp.MustCompile(`Manual verification — round \d+/\d+: infrastructure error`)
	roundBootstrap = regexp.MustCompile(`Manual verification — no round: Bootstrap state`)
)

// LastRound is the last round the Daemon, login, posted on the PR; false
// when it has posted none. Anyone else's comment is not a round.
func LastRound(comments []github.Comment, login string) (Round, bool) {
	for i := len(comments) - 1; i >= 0; i-- {
		c := comments[i]
		if !IsVerifyRound(c.Body) || !strings.EqualFold(c.Author, login) {
			continue
		}
		return roundOf(c.Body), true
	}
	return Round{}, false
}

func roundOf(body string) Round {
	var r Round
	if roundBootstrap.MatchString(body) {
		return Round{Bootstrap: true}
	}
	if m := roundNumber.FindStringSubmatch(body); m != nil {
		r.N, r.Max = atoi(m[1]), atoi(m[2])
	}
	if roundInfra.MatchString(body) {
		r.Infra = true
		return r
	}
	if m := roundTally.FindStringSubmatch(body); m != nil {
		r.Pass, r.Total, r.Deferred, r.Fail = atoi(m[1]), atoi(m[2]), atoi(m[3]), atoi(m[4])
	}
	return r
}

func atoi(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}
