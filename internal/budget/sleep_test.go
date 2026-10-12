package budget

import (
	"testing"
	"time"
)

// Ported from lib/sleep-planner.test.sh.
func TestPlanSleep(t *testing.T) {
	rfc := func(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000Z") }
	tests := []struct {
		name     string
		resetAt  string
		want     int
		degraded bool
	}{
		{"reset in the future sleeps until reset", rfc(now.Add(2 * time.Hour)), 7200, false},
		{"reset already passed polls immediately", rfc(now.Add(-time.Hour)), 0, false},
		{"missing reset uses degraded default sleep", "", 18000, true},
		{"unparseable reset uses degraded default sleep", "not-a-timestamp", 18000, true},
		{"a reset given as an epoch is read as one", "1789437600", 7200, false},
		{"a reset with an offset, as the usage endpoint gives it", "2026-09-15T05:10:00+00:00", 18600, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plan := PlanSleep(tt.resetAt, now)
			if plan.SleepSecs != tt.want || plan.Degraded != tt.degraded {
				t.Errorf("plan = %+v, want sleep %d, degraded %v", plan, tt.want, tt.degraded)
			}
			if plan.PollIntervalSecs != 300 || plan.PollMaxAttempts != 12 {
				t.Errorf("the poll plan = %+v", plan)
			}
		})
	}
}
