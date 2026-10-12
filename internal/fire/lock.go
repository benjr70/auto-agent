package fire

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// How often and how far apart hold tries before it takes the lock as held.
const (
	holdTries = 3
	holdWait  = 50 * time.Millisecond
)

// lockName is the file in the State dir a running Fire holds a lock on.
const lockName = "fire.lock"

// hold marks a Fire as running in the State dir, for as long as the process
// lives or until release is called: the kernel drops the lock when the
// process dies, however it dies. alone is false when another Fire holds it.
func hold(stateDir string) (release func(), alone bool, err error) {
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return nil, false, err
	}
	file, err := os.OpenFile(filepath.Join(stateDir, lockName), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, false, err
	}
	// Live holds the file for an instant to look; a Fire does not give way
	// to that.
	for try := 0; ; try++ {
		err = syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return func() { file.Close() }, true, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) || try == holdTries {
			break
		}
		time.Sleep(holdWait)
	}
	file.Close()
	if errors.Is(err, syscall.EWOULDBLOCK) {
		return func() {}, false, nil
	}
	return nil, false, err
}

// Live reports whether a Fire is running in the State dir now. It leaves
// nothing behind: a State dir that does not exist has no Fire in it.
func Live(stateDir string) bool {
	file, err := os.Open(filepath.Join(stateDir, lockName))
	if err != nil {
		return false
	}
	defer file.Close()
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_SH|syscall.LOCK_NB); err != nil {
		return errors.Is(err, syscall.EWOULDBLOCK)
	}
	return false
}
