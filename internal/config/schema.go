package config

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// harnessSchemaJSON is a copy of plugin/schema/harness.schema.json, which the
// bash loader reads until the cut-over (ADR 0014). A test keeps the two equal.
//
//go:embed harness.schema.json
var harnessSchemaJSON []byte

var harnessSchema = func() map[string]any {
	var s map[string]any
	if err := json.Unmarshal(harnessSchemaJSON, &s); err != nil {
		panic("config: the embedded Harness config schema is not JSON: " + err.Error())
	}
	return s
}()

// validate checks an instance against the subset of JSON Schema draft-07 the
// Harness config schema uses, and returns one problem per failure as
// "<json-pointer>: <message>". It is plugin/schema/validate.jq in Go: the
// same keywords, the same messages.
func validate(inst any, schema map[string]any) []string {
	v := validator{}
	v.check(inst, schema, "")
	for i, p := range v.problems {
		if strings.HasPrefix(p, ": ") {
			v.problems[i] = "/" + p
		}
	}
	return v.problems
}

type validator struct{ problems []string }

func (v *validator) addf(pointer, format string, args ...any) {
	v.problems = append(v.problems, pointer+": "+fmt.Sprintf(format, args...))
}

// passes reports whether inst satisfies schema, without recording problems.
func passes(inst any, schema any) bool {
	sub := validator{}
	sub.check(inst, schema, "")
	return len(sub.problems) == 0
}

func (v *validator) check(inst any, schema any, p string) {
	if allowed, ok := schema.(bool); ok {
		if !allowed {
			v.addf(p, "not allowed")
		}
		return
	}
	s, _ := schema.(map[string]any)

	if t, ok := s["type"]; ok {
		types := stringList(t)
		matched := false
		for _, want := range types {
			matched = matched || typeMatches(inst, want)
		}
		if !matched {
			v.addf(p, "expected %s, got %s", strings.Join(types, " or "), typeName(inst))
		}
	}
	if enum, ok := s["enum"].([]any); ok {
		found := false
		quoted := make([]string, len(enum))
		for i, e := range enum {
			found = found || jsonEqual(e, inst)
			quoted[i] = toJSON(e)
		}
		if !found {
			v.addf(p, "must be one of %s, got %s", strings.Join(quoted, ", "), toJSON(inst))
		}
	}
	if c, ok := s["const"]; ok && !jsonEqual(c, inst) {
		v.addf(p, "must equal %s", toJSON(c))
	}

	switch inst := inst.(type) {
	case map[string]any:
		v.checkObject(inst, s, p)
	case []any:
		if min, ok := s["minItems"].(float64); ok && float64(len(inst)) < min {
			v.addf(p, "needs at least %s item(s)", number(min))
		}
		if items, ok := s["items"]; ok {
			for i, item := range inst {
				v.check(item, items, p+"/"+strconv.Itoa(i))
			}
		}
	case float64:
		if min, ok := s["minimum"].(float64); ok && inst < min {
			v.addf(p, "must be >= %s", number(min))
		}
		if max, ok := s["maximum"].(float64); ok && inst > max {
			v.addf(p, "must be <= %s", number(max))
		}
	case string:
		if min, ok := s["minLength"].(float64); ok && float64(utf8.RuneCountInString(inst)) < min {
			v.addf(p, "must be at least %s character(s)", number(min))
		}
		if pattern, ok := s["pattern"].(string); ok && !regexp.MustCompile(pattern).MatchString(inst) {
			v.addf(p, "%q does not match %s", inst, pattern)
		}
	}

	if all, ok := s["allOf"].([]any); ok {
		for _, sub := range all {
			v.check(inst, sub, p)
		}
	}
	if anyOf, ok := s["anyOf"].([]any); ok {
		matched := false
		for _, sub := range anyOf {
			matched = matched || passes(inst, sub)
		}
		if !matched {
			v.addf(p, "matches none of the allowed shapes")
		}
	}
	if oneOf, ok := s["oneOf"].([]any); ok {
		matches := 0
		shapes := make([]string, len(oneOf))
		for i, sub := range oneOf {
			if passes(inst, sub) {
				matches++
			}
			if m, ok := sub.(map[string]any); ok {
				shapes[i] = strings.Join(stringList(m["required"]), "+")
			}
		}
		switch {
		case matches == 0:
			v.addf(p, "matches none of the allowed shapes (%s)", strings.Join(shapes, " | "))
		case matches > 1:
			v.addf(p, "matches more than one shape; declare exactly one of %s", strings.Join(shapes, " | "))
		}
	}
	if not, ok := s["not"]; ok && passes(inst, not) {
		v.addf(p, "must not match the forbidden shape")
	}
}

func (v *validator) checkObject(inst map[string]any, s map[string]any, p string) {
	for _, r := range stringList(s["required"]) {
		if _, ok := inst[r]; !ok {
			v.addf(p, "missing required key %q", r)
		}
	}
	properties, _ := s["properties"].(map[string]any)
	for _, k := range sortedKeys(properties) {
		if value, ok := inst[k]; ok {
			v.check(value, properties[k], p+"/"+k)
		}
	}
	additional, hasAdditional := s["additionalProperties"]
	for _, k := range sortedKeys(inst) {
		if _, declared := properties[k]; declared || !hasAdditional {
			continue
		}
		switch additional := additional.(type) {
		case bool:
			if !additional {
				v.addf(p, "unknown key %q", k)
			}
		default:
			v.check(inst[k], additional, p+"/"+k)
		}
	}
	if names, ok := s["propertyNames"]; ok {
		for _, k := range sortedKeys(inst) {
			v.check(k, names, p+"/"+k)
		}
	}
}

// typeName is the JSON Schema type of a decoded value; a whole number is an
// integer.
func typeName(inst any) string {
	switch inst := inst.(type) {
	case nil:
		return "null"
	case bool:
		return "boolean"
	case string:
		return "string"
	case float64:
		if inst == math.Floor(inst) {
			return "integer"
		}
		return "number"
	case []any:
		return "array"
	default:
		return "object"
	}
}

func typeMatches(inst any, want string) bool {
	got := typeName(inst)
	return got == want || (want == "number" && got == "integer")
}

func stringList(v any) []string {
	switch v := v.(type) {
	case string:
		return []string{v}
	case []any:
		out := make([]string, 0, len(v))
		for _, e := range v {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func toJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func jsonEqual(a, b any) bool { return toJSON(a) == toJSON(b) }

func number(f float64) string { return strconv.FormatFloat(f, 'f', -1, 64) }

// schemaDefault reads the default the schema declares for the key at path
// (slash-separated, as in "rounds/pr_watch") into out. Defaults live in the
// schema alone.
func schemaDefault(path string, out any) {
	node := harnessSchema
	for _, key := range strings.Split(path, "/") {
		properties, _ := node["properties"].(map[string]any)
		node, _ = properties[key].(map[string]any)
	}
	value, ok := node["default"]
	if !ok {
		panic("config: the Harness config schema declares no default for " + path)
	}
	if err := json.Unmarshal([]byte(toJSON(value)), out); err != nil {
		panic("config: the schema default for " + path + ": " + err.Error())
	}
}
