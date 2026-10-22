package fire

import (
	"testing"

	"github.com/benjr70/auto-agent/internal/config"
)

func TestGraduatedPriority(t *testing.T) {
	project := func(order ...string) config.Pick {
		return config.Pick{Shape: config.PickProject, Project: &config.Project{Number: 7, PriorityField: "Priority", Order: order}}
	}
	for name, c := range map[string]struct {
		pick config.Pick
		want string
	}{
		"a Project pick: the second of the configured order":    {project("P0", "P1", "P2"), "P1"},
		"a Project pick with one Priority: that one":            {project("Now"), "Now"},
		"a Project pick with no order: none, and Add says why":  {project(), ""},
		"a label-only pick: none, as it has no Priority to set": {config.Pick{Shape: config.PickLabels}, ""},
	} {
		t.Run(name, func(t *testing.T) {
			if got := graduatedPriority(c.pick); got != c.want {
				t.Errorf("graduatedPriority = %q, want %q", got, c.want)
			}
		})
	}
}
