package ruling

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// Normalized are the decisions with every option's letter filled in, upper
// case: the one shape a request is composed from and read back as.
func Normalized(decisions []Decision) []Decision {
	all := letters(decisions)
	normalized := make([]Decision, len(decisions))
	for i, d := range decisions {
		d.Options = append([]Option{}, d.Options...)
		for j := range d.Options {
			d.Options[j].Letter = all[i][j]
		}
		normalized[i] = d
	}
	return normalized
}

// recommended is a decision's recommended option: the first marked, else its
// first.
func recommended(d Decision) Option {
	for _, o := range d.Options {
		if o.Recommended {
			return o
		}
	}
	return d.Options[0]
}

// Check reports a decision the human could not answer in one letter: one
// with no title, fewer than two options, an option with no text, a letter
// used twice, or not exactly one option recommended.
func (d Decision) Check() error {
	switch {
	case strings.TrimSpace(d.Title) == "":
		return errors.New("a decision has no title")
	case len(d.Options) < 2:
		return fmt.Errorf("decision %q offers fewer than two options", d.Title)
	}
	seen, recommendations := map[string]bool{}, 0
	for i, letter := range letters([]Decision{d})[0] {
		o := d.Options[i]
		switch {
		case strings.TrimSpace(o.Text) == "":
			return fmt.Errorf("decision %q has an option with no text", d.Title)
		case letter == "" || len(letter) != 1 || letter[0] < 'A' || letter[0] > 'Z':
			return fmt.Errorf("decision %q has an option lettered %q, not A to Z", d.Title, letter)
		case seen[letter]:
			return fmt.Errorf("decision %q uses the letter %s twice", d.Title, letter)
		}
		seen[letter] = true
		if o.Recommended {
			recommendations++
		}
	}
	if recommendations != 1 {
		return fmt.Errorf("decision %q recommends %d options, not one", d.Title, recommendations)
	}
	return nil
}

// Compose is the Ruling request comment, in the prototype's shape: the
// marker, the heading, how to reply, a section per decision with its option
// table, a footer stating the head's evidence and what the all-recommended
// reply does, and last a hidden block carrying the decisions themselves, so
// that Read can read them back from GitHub. supersedes is the earlier
// request still open this one replaces, its decisions carried first under
// the same numbers; 0 when there is none.
func Compose(decisions []Decision, head, evidence string, supersedes int64) (string, error) {
	n := len(decisions)
	if n == 0 {
		return "", errors.New("ruling: no decisions to ask")
	}
	normalized := Normalized(decisions)
	all, ends := make([]string, n), "leaves the PR ready to merge with no further Fire."
	for i, d := range normalized {
		if len(d.Options) == 0 {
			return "", fmt.Errorf("ruling: decision %d offers no option", i+1)
		}
		pick := recommended(d)
		all[i] = fmt.Sprintf("%d%s", i+1, pick.Letter)
		if pick.Fix {
			ends = "applies the fix and re-runs verification in one more Fire."
		}
	}
	reply := strings.Join(all, " ")
	noun := "decisions"
	if n == 1 {
		noun = "decision"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s head=%s decisions=%d -->\n", RequestMarker, head, n)
	fmt.Fprintf(&b, "## 🧑‍⚖️ Ruling request · %d %s\n\n", n, noun)
	fmt.Fprintf(&b, "Reply with one line, one letter per decision: `%s`. Nothing else in the reply is read.\n\n", reply)
	if supersedes != 0 {
		fmt.Fprintf(&b, "This request supersedes the earlier one (comment %d): its decisions are carried here under the same numbers, with the new ones after. Reply to this one — a reply to the earlier one is not read.\n\n", supersedes)
	}
	for i, d := range normalized {
		fmt.Fprintf(&b, "### %d. %s\n\n%s\n\n- **Now:** %s\n- **Reviewer wants:** %s\n- **Why this is yours:** %s\n\n| | Option | What it costs |\n|---|---|---|\n",
			i+1, d.Title, d.Scenario, d.Now, d.Wants, d.Why)
		for _, o := range d.Options {
			mark := ""
			if o.Recommended {
				mark = " _Recommended._"
			}
			fmt.Fprintf(&b, "| **%s** | %s | %s%s |\n", o.Letter, o.Text, o.Cost, mark)
		}
		b.WriteString("\n")
	}
	data, err := json.Marshal(normalized)
	if err != nil {
		return "", err
	}
	fmt.Fprintf(&b, "---\n\nHead `%s`: %s. A reply of `%s` %s\n", head, evidence, reply, ends)
	fmt.Fprintf(&b, "%s %s -->\n", DecisionsMarker, base64.StdEncoding.EncodeToString(data))
	return b.String(), nil
}

// Applied is what was done for one decision of a Ruling, as the applied
// comment says it.
type Applied struct {
	N      int
	Letter string
	// Summary says what the chosen option settled.
	Summary string
	// SHA is the commit its fix landed in; empty when it changed no code.
	SHA string
	// Thread is the thread it came from, as path:line; empty when none.
	Thread string
}

// ComposeApplied is the comment that says a Ruling was applied, in the
// prototype's shape: its marker, a line per decision, and what was verified
// on the head. evidence empty says nothing was re-run.
func ComposeApplied(results []Applied, head, ruling, evidence string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s head=%s ruling=%q -->\n## ✅ Ruling applied · `%s`\n\n", AppliedMarker, head, ruling, ruling)
	for _, r := range results {
		fmt.Fprintf(&b, "%d. **%s** — %s", r.N, r.Letter, r.Summary)
		switch {
		case r.SHA != "" && r.Thread != "":
			fmt.Fprintf(&b, " Fixed in `%s`; thread on `%s` resolved.\n", r.SHA, r.Thread)
		case r.SHA != "":
			fmt.Fprintf(&b, " Fixed in `%s`.\n", r.SHA)
		case r.Thread != "":
			fmt.Fprintf(&b, " Thread on `%s` resolved, no change.\n", r.Thread)
		default:
			b.WriteString(" No change.\n")
		}
	}
	if evidence == "" {
		fmt.Fprintf(&b, "\nVerification was not re-run on `%s`: this build checks neither CI nor the checklist yet.\n", head)
	} else {
		fmt.Fprintf(&b, "\nVerification re-ran on `%s`: %s\n", head, evidence)
	}
	return b.String()
}

// ThreadReply is the reply a thread a Ruling settled gets, after the
// Daemon's ruling marker: what was chosen, and the commit it landed in, if
// any.
func ThreadReply(n int, letter, summary, sha string) string {
	if sha != "" {
		return fmt.Sprintf("Ruling %d%s: %s Fixed in `%s`; resolving.", n, letter, summary, sha)
	}
	return fmt.Sprintf("Ruling %d%s: %s Resolving, no change.", n, letter, summary)
}

// Nudge is the one comment a reply that is not a full Ruling gets: why
// nothing was applied, and the letters each decision takes.
func Nudge(decisions []Decision, reason string) string {
	normalized := Normalized(decisions)
	all := make([]string, len(normalized))
	var expected []string
	for i, d := range normalized {
		all[i] = fmt.Sprintf("%d%s", i+1, recommended(d).Letter)
		var letters []string
		for _, o := range d.Options {
			letters = append(letters, o.Letter)
		}
		choice := letters[len(letters)-1]
		if len(letters) > 1 {
			choice = strings.Join(letters[:len(letters)-1], ", ") + " or " + choice
		}
		expected = append(expected, fmt.Sprintf("- decision %d: %s", i+1, choice))
	}
	return fmt.Sprintf("%s\nThat reply is not a Ruling (%s), so nothing was applied. Reply with one line of decision-letter pairs, nothing else — for example `%s`:\n\n%s",
		NudgeMarker, reason, strings.Join(all, " "), strings.Join(expected, "\n"))
}
