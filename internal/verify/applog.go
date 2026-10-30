package verify

import (
	"errors"
	"io"
	"os"
)

// AppLogCap is how much of an Electron app's log is kept: its last 10 MB.
const AppLogCap = 10 << 20

// appLogSlack is how far past the cap a log may grow before it is cut back,
// so that a chatty app does not rewrite its log on every line.
const appLogSlack = AppLogCap / 10

// appLog is an app's log on the Host: appended to, and cut back to its last
// AppLogCap bytes whenever it grows past the cap and the slack, and when it
// is closed.
type appLog struct {
	f    *os.File
	size int64
}

// openAppLog opens the log at path to append to it.
func openAppLog(path string) (*appLog, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	size, err := f.Seek(0, io.SeekEnd)
	if err != nil {
		f.Close()
		return nil, err
	}
	return &appLog{f: f, size: size}, nil
}

func (l *appLog) Write(p []byte) (int, error) {
	n, err := l.f.Write(p)
	l.size += int64(n)
	if err == nil && l.size > AppLogCap+appLogSlack {
		err = l.cut()
	}
	return n, err
}

// cut keeps only the log's last AppLogCap bytes.
func (l *appLog) cut() error {
	tail := make([]byte, AppLogCap)
	if _, err := l.f.ReadAt(tail, l.size-AppLogCap); err != nil {
		return err
	}
	if _, err := l.f.WriteAt(tail, 0); err != nil {
		return err
	}
	if err := l.f.Truncate(AppLogCap); err != nil {
		return err
	}
	l.size = AppLogCap
	_, err := l.f.Seek(AppLogCap, io.SeekStart)
	return err
}

// Close cuts the log back to the cap and closes it.
func (l *appLog) Close() error {
	var err error
	if l.size > AppLogCap {
		err = l.cut()
	}
	return errors.Join(err, l.f.Close())
}

// copyAppLog copies what the app writes to r into its log until every
// process holding the pipe's other end is gone, then closes both and closes
// done. A log that cannot be written is given up on, never the app: what it
// writes after is read and dropped.
func copyAppLog(r *os.File, log *appLog, done chan<- struct{}) {
	if _, err := io.Copy(log, r); err != nil {
		io.Copy(io.Discard, r)
	}
	r.Close()
	log.Close()
	close(done)
}
