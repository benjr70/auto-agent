package budget

import (
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/benjr70/auto-agent/internal/step"
)

// Outcome is what the Budget gate keeps of how a Step ended: enough to hold
// the next Step until a limit frees, and the login-expiry notice Claude
// printed.
type Outcome struct {
	Status string `json:"status"`
	// ResetAt is when the limit that cut the Step off frees; empty when
	// the Step was not cut off or nothing said when.
	ResetAt string `json:"resetAt"`
	// Source is where the exhaustion was read from: the stream's rejected
	// rate-limit event, or the limit strings in the output.
	Source Text `json:"source"`
	// LimitType is the limit named: a window ("five_hour"), "session",
	// "weekly" or a model.
	LimitType  Text     `json:"limitType"`
	ObservedAt string   `json:"observedAt"`
	Warnings   []string `json:"warnings"`
}

var (
	pipedEpoch  = regexp.MustCompile(`(?i)limit reached\|([0-9]+)`)
	isoInstant  = regexp.MustCompile(`(?i)[0-9]{4}-[0-9]{2}-[0-9]{2}[T ][0-9]{2}:[0-9]{2}(:[0-9]{2})?([.][0-9]+)?Z?`)
	bareEpoch   = regexp.MustCompile(`[0-9]{10,}`)
	resetWord   = regexp.MustCompile(`(?i)reset`)
	resetsSpace = regexp.MustCompile(`(?i)resets?\s`)
	clockTime   = regexp.MustCompile(`(?i)([0-9]{1,2})(:([0-9]{2}))? ?(am|pm)`)
	zoneName    = regexp.MustCompile(`\(([A-Za-z_]+(/[A-Za-z_+-]+)+)\)`)
	namedLimit  = regexp.MustCompile(`(?i)hit your ([a-z0-9 .-]+) limit`)
	weeklyLimit = regexp.MustCompile(`(?i)weekly limit`)
	sessLimit   = regexp.MustCompile(`(?i)session limit`)
	// The documented 3-day login-expiry notice. It never blocks a call.
	expiryNotice = regexp.MustCompile(`(?i)log.?in.*expir|expir.*log.?in|credential.*expir`)
	rateLimitish = regexp.MustCompile(`(?i)rate.?limit`)
)

// scrapedISO is the form a scraped reset is kept in.
func scrapedISO(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000Z") }

// OutcomeOf reads a Step's run for the Budget gate. The runner has already
// classified it; this adds when an exhausted run's limit frees and which
// limit it was. A rejected rate-limit event is authoritative; the limit
// strings are the fallback.
func OutcomeOf(run step.Run, now time.Time) Outcome {
	out := Outcome{Status: string(run.Outcome), ObservedAt: iso(now), Warnings: []string{}}
	for _, line := range strings.Split(run.Output, "\n") {
		if len(out.Warnings) < 3 && expiryNotice.MatchString(line) && !rateLimitish.MatchString(line) {
			out.Warnings = append(out.Warnings, strings.TrimSpace(line))
		}
	}
	if run.Outcome != step.Exhausted {
		return out
	}
	if n := len(run.RateLimits); n > 0 {
		var event struct {
			Info struct {
				Status        string `json:"status"`
				ResetsAt      *int64 `json:"resetsAt"`
				RateLimitType Text   `json:"rateLimitType"`
			} `json:"rate_limit_info"`
		}
		if json.Unmarshal(run.RateLimits[n-1], &event) == nil && event.Info.Status == "rejected" {
			out.Source, out.LimitType = SensorStreamEvents, event.Info.RateLimitType
			if event.Info.ResetsAt != nil {
				out.ResetAt = iso(time.Unix(*event.Info.ResetsAt, 0))
			}
			return out
		}
	}
	out.Source = SensorLimitStrings
	out.ResetAt = scrapeReset(run.Output, now)
	out.LimitType = Text(scrapeLimitType(run.Output))
	return out
}

// scrapeReset finds the reset instant in an exhaustion notice: Claude's
// pipe-delimited epoch, an ISO instant or a bare epoch on a line that says
// "reset", then a wall-clock time on such a line. Empty when there is none.
func scrapeReset(text string, now time.Time) string {
	if m := pipedEpoch.FindStringSubmatch(text); m != nil {
		return epochISO(m[1])
	}
	var resetLines []string
	for _, line := range strings.Split(text, "\n") {
		if resetWord.MatchString(line) {
			resetLines = append(resetLines, line)
		}
	}
	for _, line := range resetLines {
		if m := isoInstant.FindString(line); m != "" {
			return parseInstant(m)
		}
	}
	for _, line := range resetLines {
		if m := bareEpoch.FindString(line); m != "" {
			return epochISO(m)
		}
	}
	for _, line := range resetLines {
		if resetsSpace.MatchString(line) {
			return clockReset(line, now)
		}
	}
	return ""
}

func epochISO(digits string) string {
	n, err := strconv.ParseInt(digits, 10, 64)
	if err != nil {
		return ""
	}
	return scrapedISO(time.Unix(n, 0))
}

// parseInstant reads an ISO instant with or without seconds, a fraction or
// a Z; one with no zone is UTC.
func parseInstant(text string) string {
	text = strings.TrimSuffix(strings.Replace(strings.ToUpper(text), " ", "T", 1), "Z")
	for _, layout := range []string{"2006-01-02T15:04:05.999999999", "2006-01-02T15:04:05", "2006-01-02T15:04"} {
		if t, err := time.Parse(layout, text); err == nil {
			return scrapedISO(t)
		}
	}
	return ""
}

// clockReset resolves a wall-clock notice ("resets 10:50pm
// (America/New_York)") to the next time the clock in that zone reads so.
func clockReset(line string, now time.Time) string {
	m := clockTime.FindStringSubmatch(line)
	if m == nil {
		return ""
	}
	hour, _ := strconv.Atoi(m[1])
	minute, _ := strconv.Atoi(m[3])
	switch meridiem := strings.ToLower(m[4]); {
	case meridiem == "pm" && hour != 12:
		hour += 12
	case meridiem == "am" && hour == 12:
		hour = 0
	}
	zone := time.UTC
	if z := zoneName.FindStringSubmatch(line); z != nil {
		if loaded, err := time.LoadLocation(z[1]); err == nil {
			zone = loaded
		}
	}
	local := now.In(zone)
	at := time.Date(local.Year(), local.Month(), local.Day(), hour, minute, 0, 0, zone)
	if !at.After(now) {
		at = at.Add(24 * time.Hour)
	}
	return scrapedISO(at)
}

// scrapeLimitType is the limit a notice names: "hit your Fable limit" is
// fable; empty when it names none.
func scrapeLimitType(text string) string {
	switch {
	case namedLimit.MatchString(text):
		return strings.ToLower(namedLimit.FindStringSubmatch(text)[1])
	case weeklyLimit.MatchString(text):
		return ScopeWeekly
	case sessLimit.MatchString(text):
		return ScopeSession
	}
	return ""
}
