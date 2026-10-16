// Package lock holds Odin's file locks: a run's, a project's, and a merge target's (spec §4.5).
// They are flock locks, so the kernel drops them when the process ends, and a killed ygg never
// leaves one behind.
package lock

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// ErrHeld means another process holds the lock.
var ErrHeld = errors.New("another process holds the lock")

// Take takes the lock at path for this process, creating the file and its directory, and returns
// the function that releases it. It never waits: a held lock is ErrHeld.
func Take(path string) (release func(), err error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("%s: %w", path, ErrHeld)
		}
		return nil, err
	}
	return func() { f.Close() }, nil
}

// Held reports whether a process holds the lock at path. It reads /proc/locks and neither takes
// the lock nor creates the file, so a check never makes another process's Take fail.
func Held(path string) bool {
	var st syscall.Stat_t
	if err := syscall.Stat(path, &st); err != nil {
		return false
	}
	major := (st.Dev>>8)&0xfff | (st.Dev>>32)&^0xfff
	minor := st.Dev&0xff | (st.Dev>>12)&^0xff
	id := fmt.Sprintf("%02x:%02x:%d", major, minor, st.Ino)
	f, err := os.Open("/proc/locks")
	if err != nil {
		return false
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		// 1: FLOCK  ADVISORY  WRITE 4242 08:20:12345 0 EOF
		fields := strings.Fields(sc.Text())
		if len(fields) >= 6 && fields[1] == "FLOCK" && fields[5] == id {
			return true
		}
	}
	return false
}
