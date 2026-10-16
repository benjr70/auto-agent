package fire

import "testing"

// Ported from lib/arbiter-verdicts.test.sh: av_next_round.
func TestAnotherRound(t *testing.T) {
	for _, c := range []struct {
		name                     string
		done, rounds, ruledFixes int
		next                     int
	}{
		{"cap 1, round 1, nothing ruled fix: no round remains", 1, 1, 0, 0},
		{"cap 1, round 1, one ruled fix: the ruled round is guaranteed (round 2 runs)", 1, 1, 1, 2},
		{"cap 1, after the ruled round 2: no round remains — the guarantee is one round, not a new cap", 2, 1, 0, 0},
		{"cap 3, round 1 -> 2", 1, 3, 0, 2},
		{"cap 3, round 2 -> 3", 2, 3, 0, 3},
		{"cap 3, round 3 -> cap", 3, 3, 0, 0},
		{"a Fire with one implementer round and one Arbiter run has used one round: the Arbiter and its dismissals are not inputs, so the next round is 2 under a cap of 2", 1, 2, 0, 2},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := anotherRound(c.done, c.rounds, c.ruledFixes); got != (c.next != 0) {
				t.Errorf("anotherRound = %v, want round %d", got, c.next)
			}
		})
	}
}
