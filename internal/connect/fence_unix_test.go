//go:build !windows

package connect

import "syscall"

func syscallMkfifo(path string) error { return syscall.Mkfifo(path, 0o600) }
