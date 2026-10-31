package fire

import (
	"context"
	"fmt"
	"strings"

	"github.com/benjr70/auto-agent/internal/config"
	"github.com/benjr70/auto-agent/internal/disk"
	"github.com/benjr70/auto-agent/internal/journal"
)

// PhaseDisk: the Disk guard found the Host short of the free space a round
// needs, and the Fire ended before the round started.
const PhaseDisk = "disk"

// diskShort is the Disk guard, asked before a round boots its environment:
// whether the State dir's filesystem has the free space the head's
// verification.hermetic.disk_free declares one round needs. A Host that is
// short runs the cleanup-first hook and looks again; still short, the Fire
// ends here, before the round starts: nothing is posted, no round is
// counted and no attempt is spent, and the record says what was free and
// what is needed, which the Daemon waits on. A free space that cannot be
// read holds nothing back: the round goes, with a warning.
func (f *Fire) diskShort(ctx context.Context, record *journal.Record, head config.Harness) bool {
	need := head.Verification.Hermetic.DiskFreeBytes()
	free, err := f.freeSpace()
	if err != nil || free >= need {
		f.unreadFree(record, err)
		return false
	}
	for _, clean := range f.cleanupFirst(head) {
		if err := clean(ctx); err != nil {
			record.Warnings = append(record.Warnings, "cleaning up for the Disk guard: "+err.Error())
		}
	}
	if free, err = f.freeSpace(); err != nil || free >= need {
		f.unreadFree(record, err)
		return false
	}
	record.Phase, record.Disk = PhaseDisk, &journal.Disk{Free: free, Need: need}
	return true
}

// unreadFree warns that the Disk guard could not read the free space.
func (f *Fire) unreadFree(record *journal.Record, err error) {
	if err != nil {
		record.Warnings = append(record.Warnings, "the Disk guard let the round go unchecked: "+err.Error())
	}
}

// cleanupFirst is the cleanup-first hook: what the Disk guard runs, in
// order, on a Host short of space, before it looks again. Each removes
// Footprint nothing needs; one that fails never stops the others. The
// Sweep's own cleanup comes first: the State dir's records and evidence
// past their window, and every environment the head's Environment provider
// owns, as nothing of this round is up yet.
func (f *Fire) cleanupFirst(head config.Harness) []func(context.Context) error {
	return []func(context.Context) error{
		f.sweep,
		func(ctx context.Context) error { return f.sweepEnvironments(ctx, head) },
	}
}

// freeSpace is the free space on the State dir's filesystem.
func (f *Fire) freeSpace() (uint64, error) {
	read := f.FreeSpace
	if read == nil {
		read = disk.Free
	}
	return read(f.Journal.StateDir)
}

// filledNote is what a round's infrastructure error adds when the disk
// filled during the round: the error says no space was left, or the Host
// is now short of what hermetic declares a round needs. Either way the
// declared need may be too small. Empty when the Disk guard never let the
// round go, hermetic nil, or nothing says the disk filled.
func (f *Fire) filledNote(hermetic *config.Hermetic, detail string) string {
	if hermetic == nil {
		return ""
	}
	need := hermetic.DiskFreeBytes()
	free, err := f.freeSpace()
	short := err == nil && free < need
	if !short && !strings.Contains(strings.ToLower(detail), "no space left on device") {
		return ""
	}
	now := ""
	if short {
		now = fmt.Sprintf("only %s free now; ", disk.Size(free))
	}
	return fmt.Sprintf(" — the disk filled during the round (%sverification.hermetic.disk_free declares %s a round needs, which may be too small)", now, hermetic.DiskFree)
}
