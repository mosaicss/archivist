package cmd_test

import (
	"errors"
	"testing"

	"github.com/mosaicss/archivist/internal/cmd"
)

func TestConnectValidatesCodexFlags(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	for _, args := range [][]string{
		{"connect", "--codex-effort", "turbo"},
		{"connect", "--codex-model", "-x"},
		{"connect", "--codex-model", "a b"},
	} {
		_, err := runAuthCmd(t, args...)
		var exitErr *cmd.ExitError
		if err == nil || (errors.As(err, &exitErr) && exitErr.Code != cmd.ExitUsageError) {
			t.Errorf("%v: %v", args, err)
		}
	}
}
