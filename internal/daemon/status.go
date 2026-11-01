package daemon

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/benjr70/auto-agent/internal/journal"
)

// TailLines is how many of its log lines the Daemon keeps for the Dashboard.
const TailLines = 30

// What the Daemon is doing now.
const (
	// StateFiring: a Fire is running.
	StateFiring = "firing"
	// StateIdle: the last Fire found no work, or failed, and the Daemon
	// waits to derive again.
	StateIdle = "idle"
	// StateHeld: the Budget gate holds the Daemon to a reset.
	StateHeld = "held"
	// StateParked: Claude's credential is dead; the Daemon probes it.
	StateParked = "parked"
	// StateDisk: the Disk guard found the Host short of the free space a
	// round needs; the Daemon checks again.
	StateDisk = "disk"
)

// State is what the Daemon is doing now, and since when.
type State struct {
	State  string
	Detail string
	At     time.Time
	// ResetAt is the reset a held Daemon waits for; empty when it names
	// none.
	ResetAt string
	// Fails is how many Fires in a row have failed.
	Fails int
	// Parked is the park the Daemon is in; nil when it is not Parked.
	Parked *journal.Park
	// Disk is the free space a Daemon waiting for disk has, and needs; nil
	// when it is not waiting for disk.
	Disk *journal.Disk
}

// Status is what the always-on Daemon is doing and its last log lines, held
// in memory by the run process for the Dashboard and never written to disk
// (ADR 0012). It is an io.Writer: what the Daemon logs through it is kept
// as its tail. It is safe for concurrent use; the zero value is ready.
type Status struct {
	mu      sync.Mutex
	state   State
	tail    []string
	partial string
}

// Now is what the Daemon is doing now. A Daemon that has not started has
// an empty State.
func (s *Status) Now() State {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state
}

// Tail is the Daemon's last log lines, oldest first.
func (s *Status) Tail() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string{}, s.tail...)
}

// Write keeps each whole line written as a line of the tail.
func (s *Status) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	text := s.partial + string(p)
	lines := strings.Split(text, "\n")
	s.partial = lines[len(lines)-1]
	for _, line := range lines[:len(lines)-1] {
		s.keep(line)
	}
	return len(p), nil
}

func (s *Status) keep(line string) {
	s.tail = append(s.tail, line)
	if over := len(s.tail) - TailLines; over > 0 {
		s.tail = append(s.tail[:0], s.tail[over:]...)
	}
}

// set records what the Daemon is doing now, and logs it as a line of the
// tail.
func (s *Status) set(state State) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	state.Fails = s.state.Fails
	s.state = state
	s.keep(fmt.Sprintf("[daemon %s] %s", state.At.UTC().Format(time.RFC3339), state.Detail))
}

// ended counts the Fire that ended: a failed one adds to the failures in a
// row, any other clears them.
func (s *Status) ended(record journal.Record) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if failed(record) {
		s.state.Fails++
	} else {
		s.state.Fails = 0
	}
}
