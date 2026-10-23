package run

import (
	"io/fs"
	"syscall"
)

func syscallMkfifo(path string) error { return syscall.Mkfifo(path, 0o600) }

// ownerOf is the uid that owns the file, when the system says.
func ownerOf(info fs.FileInfo) (int, bool) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return int(st.Uid), true
}
