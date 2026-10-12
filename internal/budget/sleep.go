package budget

import (
	"regexp"
	"strconv"
	"time"
)

// What the Daemon does when it does not know better.
const (
	// pollInterval is the time between checks of the gate after waking.
	pollInterval = 5 * time.Minute
	// pollMaxAttempts is how many such checks are made.
	pollMaxAttempts = 12
	// degradedSleep is the sleep when the reset is not known.
	degradedSleep = 5 * time.Hour
)

// SleepPlan is how long to sleep on a wait verdict and how to poll after:
// sleep to the estimated reset, then ask the gate again at intervals until
// the limit has really freed. That absorbs an estimate that is a little off.
type SleepPlan struct {
	SleepSecs        int `json:"sleepSecs"`
	PollIntervalSecs int `json:"pollIntervalSecs"`
	PollMaxAttempts  int `json:"pollMaxAttempts"`
	// Degraded is set when the reset was missing or unreadable.
	Degraded bool `json:"degraded"`
}

var wholeNumber = regexp.MustCompile(`^[0-9]+$`)

// PlanSleep computes the plan from the verdict's reset time, which is an ISO
// instant or an epoch. It only computes: the caller sleeps.
func PlanSleep(resetAt string, now time.Time) SleepPlan {
	plan := SleepPlan{PollIntervalSecs: int(pollInterval.Seconds()), PollMaxAttempts: pollMaxAttempts}
	var reset time.Time
	switch {
	case wholeNumber.MatchString(resetAt):
		epoch, _ := strconv.ParseInt(resetAt, 10, 64)
		reset = time.Unix(epoch, 0)
	default:
		parsed, err := time.Parse(time.RFC3339, resetAt)
		if err != nil {
			// A safe default rather than an error: the Daemon stays alive.
			plan.SleepSecs, plan.Degraded = int(degradedSleep.Seconds()), true
			return plan
		}
		reset = parsed
	}
	if reset.After(now) {
		plan.SleepSecs = int(reset.Sub(now).Seconds())
	}
	return plan
}
