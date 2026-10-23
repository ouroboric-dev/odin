package run

import "syscall"

func syscallMkfifo(path string) error { return syscall.Mkfifo(path, 0o600) }
