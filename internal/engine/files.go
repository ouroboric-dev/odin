package engine

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
)

// tooBigError is a file over the limit it was read with.
type tooBigError struct {
	path  string
	limit int64
}

func (e *tooBigError) Error() string { return fmt.Sprintf("%s is more than %d bytes", e.path, e.limit) }

// readRegular reads name, a path inside dir, as the file the listing found (listed, the FileInfo the walk
// or an Lstat took of it), and refuses anything but a regular file of at most limit bytes. A file is
// checked when its folder is listed and read later, so the check is made again on the file opened, not
// only on its path (review M-2): nothing on the way to it may lead out of dir (os.Root); name itself must
// be a regular file, not a symlink; and the file opened must be both the file looked at again and the
// listed one (os.SameFile), so a link or another file put at name in between is refused, not followed;
// and the bytes are read through the limit, so a file over it is refused, never read whole first or cut
// short. Nothing opened waits, so a pipe swapped in is refused too.
func readRegular(dir, name string, listed fs.FileInfo, limit int64) ([]byte, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	full := filepath.Join(dir, name)
	st, err := root.Lstat(name)
	switch {
	case err != nil:
		return nil, err
	case st.Mode()&fs.ModeSymlink != 0:
		return nil, fmt.Errorf("%s is a symlink", full)
	case !st.Mode().IsRegular():
		return nil, fmt.Errorf("%s is %s, not a regular file", full, st.Mode().Type())
	}
	f, err := root.OpenFile(name, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !opened.Mode().IsRegular() || !os.SameFile(st, opened) || !os.SameFile(listed, opened) {
		return nil, fmt.Errorf("%s changed while it was read: it is not the regular file that was there", full)
	}
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, &tooBigError{path: full, limit: limit}
	}
	return data, nil
}
