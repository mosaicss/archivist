//go:build windows

package connect

import "errors"

func syscallMkfifo(string) error { return errors.New("no named pipes on Windows") }
