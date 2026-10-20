package state

import "testing"

func TestTaken(t *testing.T) {
	for _, tc := range []struct {
		labels []string
		want   bool
	}{
		{nil, false},
		{[]string{LabelAFK, "wayfinder:research"}, false},
		{[]string{LabelAFK, LabelInProgress}, true},
		{[]string{LabelDone}, true},
		{[]string{LabelFailed}, true},
		{[]string{LabelPaused}, true},
	} {
		if got := Taken(tc.labels); got != tc.want {
			t.Errorf("Taken(%q) = %v, want %v", tc.labels, got, tc.want)
		}
	}
}

func TestWayfinderType(t *testing.T) {
	for _, tc := range []struct {
		labels []string
		want   string
	}{
		{nil, ""},
		{[]string{LabelAFK}, ""},
		{[]string{LabelAFK, "wayfinder:research"}, "research"},
		{[]string{"wayfinder:grilling", "wayfinder:task"}, "grilling"},
	} {
		if got := WayfinderType(tc.labels); got != tc.want {
			t.Errorf("WayfinderType(%q) = %q, want %q", tc.labels, got, tc.want)
		}
	}
}
