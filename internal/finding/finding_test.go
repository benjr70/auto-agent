package finding

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func findings(t *testing.T, raw string) []Finding {
	t.Helper()
	var list []Finding
	if err := json.Unmarshal([]byte(raw), &list); err != nil {
		t.Fatal(err)
	}
	return list
}

func kinds(list []Finding) string {
	var k []string
	for _, f := range list {
		k = append(k, string(f.Kind))
	}
	return strings.Join(k, ",")
}

// The cases carry over from lib/review-poster.test.sh under the same names.
func TestBar(t *testing.T) {
	t.Run("the bar demotes scenario-less defects and Standards findings to Review notes", func(t *testing.T) {
		got := Bar(findings(t, `[{"kind":"defect","axis":"correctness","category":"logic-error","path":"a.sh","line":3,"severity":"low","summary":"empty scenario","failure_scenario":""},
 {"kind":"defect","axis":"correctness","category":"bug","path":"a.sh","line":4,"severity":"low","summary":"says no failure","failure_scenario":"No runtime failure."},
 {"kind":"defect","axis":"correctness","category":"duplication","path":"a.sh","line":5,"severity":"high","summary":"standards","failure_scenario":"the helper is copied twice and one copy drifts"},
 {"kind":"defect","axis":"spec","category":"scope-creep","path":"a.sh","line":6,"severity":"medium","summary":"creep","failure_scenario":"an endpoint nobody asked for is reachable"},
 {"kind":"defect","axis":"correctness","category":"logic-error","path":"a.sh","line":7,"severity":"low","summary":"real","failure_scenario":"a null profile crashes the save endpoint"},
 {"kind":"defect","axis":"spec","category":"spec-mismatch","path":"a.sh","line":8,"severity":"low","summary":"contradicts AC","failure_scenario":"","quoted_requirement":"AC 2: the label is applied only when a thread was opened"},
 {"kind":"product-ambiguity","axis":"spec","category":"ambiguity","path":"a.sh","line":9,"severity":"medium","summary":"spec silent on retries","failure_scenario":""},
 {"axis":"correctness","category":"error-handling","path":"a.sh","line":10,"severity":"medium","summary":"no kind at all","failure_scenario":"a 500 on an empty body is swallowed and the caller sees 200"}]`))
		if k := kinds(got); k != "review-note,review-note,review-note,review-note,defect,defect,product-ambiguity,defect" {
			t.Fatalf("kinds after the bar: %s", k)
		}
		if !got[0].Demoted || got[4].Demoted {
			t.Error("a demoted finding is flagged demoted; an untouched one is not")
		}
		if got[4].Path != "a.sh" || got[4].Line != 7 {
			t.Error("every other field survives the bar")
		}
	})
	t.Run("the bar demotes scenario-less defects and Standards findings to Review notes: only a bare no-failure phrase demotes", func(t *testing.T) {
		got := Bar(findings(t, `[{"kind":"defect","category":"bug","failure_scenario":"Nonexistent config file makes the daemon crash at boot"},
 {"kind":"defect","category":"data-loss","failure_scenario":"Naming collision truncates the wrong table"},
 {"kind":"defect","category":"bug","failure_scenario":"No failure is visible until the cache expires, then every request 500s"},
 {"kind":"defect","category":"bug","failure_scenario":"  None. "},
 {"kind":"defect","category":"bug","failure_scenario":"n/a"},
 {"kind":"defect","category":"bug","failure_scenario":"Nothing breaks"}]`))
		if k := kinds(got); k != "defect,defect,defect,review-note,review-note,review-note" {
			t.Errorf("kinds: %s", k)
		}
	})
	t.Run("the bar demotes scenario-less defects and Standards findings to Review notes: test-coverage needs its quote, an ambiguity keeps its kind", func(t *testing.T) {
		got := Bar(findings(t, `[{"kind":"defect","category":"test-coverage","failure_scenario":"","quoted_requirement":"Every touched lib/*.sh gains its *.test.sh"},
 {"kind":"defect","category":"test-coverage","failure_scenario":"the empty case is untested and a regression there goes unnoticed"},
 {"kind":"product-ambiguity","category":"test-coverage","failure_scenario":"reading A: lib suite; reading B: e2e"},
 {"kind":"product-ambiguity","category":"duplication","failure_scenario":"A or B"}]`))
		if k := kinds(got); k != "defect,review-note,product-ambiguity,product-ambiguity" {
			t.Errorf("kinds: %s", k)
		}
	})
}

func TestSplit(t *testing.T) {
	t.Run("findings split into defects / notes / ambiguities; notes-only has no defects", func(t *testing.T) {
		routes := Split(Bar(findings(t, `[{"kind":"defect","category":"logic-error","failure_scenario":""},
 {"kind":"defect","category":"naming","failure_scenario":"x"},
 {"kind":"product-ambiguity","category":"ambiguity","failure_scenario":""}]`)))
		if len(routes.Defects) != 0 || len(routes.Notes) != 2 || len(routes.Ambiguities) != 1 {
			t.Errorf("routes = %+v", routes)
		}
		routes = Split(Bar(findings(t, `[{"kind":"defect","category":"bug","failure_scenario":"the last row is dropped on save"}]`)))
		if len(routes.Defects) != 1 {
			t.Errorf("a real defect is routed to defects: %+v", routes)
		}
	})
}

func defect(axis string, line int, severity, summary string) Finding {
	return Finding{Kind: Defect, Axis: axis, Category: "bug", Path: "lib/x.go", Line: line, Severity: severity, Summary: summary, FailureScenario: "it breaks"}
}

func TestDedupe(t *testing.T) {
	t.Run("twelve defects produce twelve threads (no cap)", func(t *testing.T) {
		var defects []Finding
		for i := 1; i <= 12; i++ {
			defects = append(defects, defect(Correctness, i*3, "low", fmt.Sprintf("defect %d", i)))
		}
		threads := Dedupe(defects)
		if len(threads) != 12 || threads[5].Line != 18 || threads[11].Line != 36 {
			t.Errorf("threads = %+v", threads)
		}
	})
	t.Run("two axes at the same path and line share one thread, correctness first", func(t *testing.T) {
		threads := Dedupe([]Finding{defect(SpecAxis, 10, "low", "AC 2 unmet"), defect(Correctness, 10, "high", "crashes")})
		if len(threads) != 1 || len(threads[0].Findings) != 2 || threads[0].Findings[0].Axis != Correctness {
			t.Fatalf("threads = %+v", threads)
		}
		if body := threads[0].Body(); strings.Count(body, "🤖 **pr-review**") != 2 || strings.Index(body, "crashes") > strings.Index(body, "AC 2 unmet") {
			t.Errorf("body:\n%s", body)
		}
	})
	t.Run("the same defect within three lines keeps the more severe one", func(t *testing.T) {
		threads := Dedupe([]Finding{defect(SpecAxis, 10, "low", "Off-by-one in the loop."), defect(Correctness, 12, "high", "off by one in the loop")})
		if len(threads) != 1 || threads[0].Line != 12 || threads[0].Findings[0].Severity != "high" {
			t.Errorf("threads = %+v", threads)
		}
		if threads := Dedupe([]Finding{defect(SpecAxis, 10, "low", "off by one"), defect(Correctness, 14, "high", "off by one")}); len(threads) != 2 {
			t.Errorf("four lines apart are two threads: %+v", threads)
		}
		if threads := Dedupe([]Finding{defect(SpecAxis, 10, "low", "off by one"), defect(Correctness, 11, "high", "nil map write")}); len(threads) != 2 {
			t.Errorf("different defects close together are two threads: %+v", threads)
		}
	})
}

func TestBody(t *testing.T) {
	thread := Thread{Path: "lib/x.go", Line: 77, Findings: []Finding{{Kind: Defect, Axis: Correctness, Category: "logic-error", Severity: "high", Summary: "inverted null check on save", FailureScenario: "a null profile crashes the save endpoint"}}}
	t.Run("rendered finding carries marker + template fields", func(t *testing.T) {
		body := thread.Body()
		for _, want := range []string{ThreadMarker + "\n", "🤖 **pr-review** · defect · correctness · logic-error · high", "inverted null check on save", "**Failure scenario:** a null profile crashes the save endpoint", "reply to dispute"} {
			if !strings.Contains(body, want) {
				t.Errorf("body lacks %q:\n%s", want, body)
			}
		}
		if !IsThread(body) {
			t.Error("the body does not read as the review's thread")
		}
	})
	t.Run("a defect that contradicts a requirement quotes it", func(t *testing.T) {
		quoted := thread
		quoted.Findings = []Finding{{Kind: Defect, Axis: SpecAxis, Category: "spec-mismatch", Severity: "low", Summary: "s", QuotedRequirement: "AC 2: the label is applied"}}
		if body := quoted.Body(); !strings.Contains(body, "**Contradicts:** > AC 2: the label is applied") {
			t.Errorf("body:\n%s", body)
		}
	})
	t.Run("unanchorable defect is posted on the file's first changed line: the body names the intended location", func(t *testing.T) {
		body := thread.FallbackBody()
		if !strings.HasPrefix(body, ThreadMarker+"\n🤖 **pr-review**") || !strings.Contains(body, "**Intended location:** `lib/x.go:77`") || !strings.Contains(body, "a null profile crashes") {
			t.Errorf("body:\n%s", body)
		}
	})
	t.Run("filter keeps only agent-marked threads", func(t *testing.T) {
		for body, want := range map[string]bool{
			"rename this variable": false,
			ThreadMarker + "\n🤖 **pr-review** · correctness · logic-error · high\n\ninverted null check": true,
			ThreadMarker + "  \nx": true,
			"re the bot's note:\n> " + ThreadMarker + "\n> 🤖 off-by-one\nI disagree, the range is inclusive": false,
		} {
			if IsThread(body) != want {
				t.Errorf("IsThread(%q) = %v", body, !want)
			}
		}
	})
}

func TestDone(t *testing.T) {
	notes := findings(t, `[{"kind":"review-note","axis":"correctness","category":"duplication","path":"a.sh","line":5,"severity":"low","summary":"helper copied twice","failure_scenario":"","demoted":true},
            {"kind":"review-note","axis":"spec","category":"test-coverage","path":"b.sh","line":9,"severity":"low","summary":"no test for the empty case","failure_scenario":""}]`)
	ambiguities := findings(t, `[{"kind":"product-ambiguity","axis":"spec","category":"ambiguity","path":"a.sh","line":9,"severity":"medium","summary":"Spec silent on retries","failure_scenario":"","quoted_requirement":""}]`)
	t.Run("done-marker comment body", func(t *testing.T) {
		body := Done(3, 1, "abc1234", Routes{}, "")
		if !strings.HasPrefix(body, "<!-- pr-review-done reviewed=abc1234 fixes=none -->\n🤖 pr-review: 3 findings, 0 fixed (reviewed abc1234)") || !IsDone(body) {
			t.Errorf("body:\n%s", body)
		}
	})
	t.Run("done-marker carries Review notes (collapsed) and ambiguities (structured)", func(t *testing.T) {
		body := Done(4, 1, "abc1234", Routes{Notes: notes, Ambiguities: ambiguities}, "")
		for _, want := range []string{
			"<!-- pr-review-done reviewed=abc1234 fixes=none -->",
			"🤖 pr-review: 4 findings, 0 fixed (reviewed abc1234) — 1 defect thread(s), 2 review note(s), 1 product ambiguity(ies)",
			"<details>", "<summary>Review notes, no action taken (2)</summary>",
			"`a.sh:5` · correctness · duplication — helper copied twice _(demoted",
			"`b.sh:9` · spec · test-coverage — no test for the empty case\n",
			"**Product ambiguities (1)**", "<!-- pr-review-ambiguities", `"summary":"Spec silent on retries"`,
		} {
			if !strings.Contains(body, want) {
				t.Errorf("body lacks %q:\n%s", want, body)
			}
		}
		if bare := Done(0, 0, "abc1234", Routes{}, ""); strings.Contains(bare, "Review notes") || strings.Contains(bare, "pr-review-ambiguities") {
			t.Errorf("no notes, no ambiguities: no empty sections\n%s", bare)
		}
	})
	t.Run("ambiguities round-trip through the done-marker body", func(t *testing.T) {
		got := Ambiguities(Done(1, 0, "abc1234", Routes{Ambiguities: ambiguities}, "extra after"))
		if len(got) != 1 || got[0].Summary != "Spec silent on retries" {
			t.Errorf("got %+v", got)
		}
		if got := Ambiguities("no block here"); got == nil || len(got) != 0 {
			t.Errorf("no block: %#v", got)
		}
		arrows := []Finding{{Kind: ProductAmbiguity, Summary: "state A --> B is undefined", FailureScenario: "reading 1 --> stay; reading 2 --> reset"}}
		body := Done(1, 0, "abc1234", Routes{Ambiguities: arrows}, "")
		_, block, _ := strings.Cut(body, ambiguitiesMarker+"\n")
		block, _, _ = strings.Cut(block, "\n-->")
		if strings.Contains(block, "-->") {
			t.Errorf("the block carries a literal -->: %s", block)
		}
		if got := Ambiguities(body); len(got) != 1 || got[0].Summary != arrows[0].Summary || got[0].FailureScenario != arrows[0].FailureScenario {
			t.Errorf("got %+v", got)
		}
	})
}
