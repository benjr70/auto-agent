// Package pidfile is the pid file of a process the Daemon launched: enough
// to find it again after the Daemon was killed, and to tell it from another
// process that was given the same pid since. A process is the one recorded
// only when its pid, its start time and the boot it started in all match.
package pidfile

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strconv"
	"strings"
	"syscall"
)

// record is what a pid file holds.
type record struct {
	Pid int `json:"pid"`
	// Start is the process's start time, in clock ticks since boot, as
	// /proc/<pid>/stat gives it.
	Start string `json:"start"`
	// Boot is the boot it started in.
	Boot string `json:"boot"`
}

// Write records the running process pid at path.
func Write(path string, pid int) error {
	start, err := startOf(pid)
	if err != nil {
		return fmt.Errorf("reading process %d's start time: %w", pid, err)
	}
	data, err := json.Marshal(record{Pid: pid, Start: start, Boot: boot()})
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o644)
}

// Stop kills the process the pid file at path names, and its process group
// when it leads one, when it is still alive and still the process recorded;
// a pid given to another process since is never signalled. The file is
// removed either way.
func Stop(path string) error {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var r record
	if json.Unmarshal(data, &r) == nil && r.Pid > 0 && same(r) {
		if group, err := syscall.Getpgid(r.Pid); err == nil && group == r.Pid {
			syscall.Kill(-r.Pid, syscall.SIGKILL)
		} else {
			syscall.Kill(r.Pid, syscall.SIGKILL)
		}
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// same reports whether the process with r's pid is the one r recorded.
func same(r record) bool {
	start, err := startOf(r.Pid)
	return err == nil && start == r.Start && boot() == r.Boot
}

// startOf is the start time of process pid, in clock ticks since boot: the
// 22nd field of its /proc stat, counted after its command name, which may
// hold spaces and parentheses.
func startOf(pid int) (string, error) {
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return "", err
	}
	at := strings.LastIndexByte(string(data), ')')
	if at < 0 {
		return "", fmt.Errorf("process %d's stat has no command name", pid)
	}
	// After the name come the state, the 3rd field, through the start, the 22nd.
	fields := strings.Fields(string(data[at+1:]))
	if len(fields) < 20 {
		return "", fmt.Errorf("process %d's stat is short", pid)
	}
	return fields[19], nil
}

// boot is this boot's id; empty when the kernel does not say.
func boot() string {
	data, _ := os.ReadFile("/proc/sys/kernel/random/boot_id")
	return strings.TrimSpace(string(data))
}
