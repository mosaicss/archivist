package cmd

import (
	"bytes"
	"strings"
	"testing"

	"github.com/mosaicss/archivist/internal/connect"
)

// --max-permission, --mode, --model and --effort (Story 78.32): the ceiling
// defaults per surface, a bad value names the valid ids, the session flags
// need --session.
func TestPermissionFlags(t *testing.T) {
	sess := func() *connect.SessionStart {
		return &connect.SessionStart{SessionID: "x", Agent: "claude", Prompt: "p"}
	}
	for _, c := range []struct {
		name, max, mode string
		model, effort   string
		session         bool
		want, wantMode  connect.Mode
		exit            int
		msg             string
	}{
		{name: "local default", want: connect.ModeAutoEdits},
		{name: "session default", session: true, want: connect.ModeFullAuto},
		{name: "local full_auto", max: "full_auto", want: connect.ModeFullAuto},
		{name: "local read_only", max: "read_only", want: connect.ModeReadOnly},
		{name: "session lowered", max: "ask", session: true, want: connect.ModeAsk},
		{name: "session mode", mode: "read_only", session: true, want: connect.ModeFullAuto, wantMode: connect.ModeReadOnly},
		{name: "bad ceiling", max: "root", exit: ExitUsageError, msg: "read_only (Read only), ask (Ask every time), auto_edits (Auto edits), full_auto (Full auto)"},
		{name: "claude mode name", max: "bypassPermissions", exit: ExitUsageError, msg: "is not a mode"},
		{name: "mode without session", mode: "ask", exit: ExitUsageError, msg: "--mode needs --session"},
		{name: "bad mode", mode: "everything", session: true, exit: ExitUsageError, msg: "--mode \"everything\" is not a mode"},
		{name: "session model and effort", model: "claude-opus-5-5[1m]", effort: "xhigh", session: true, want: connect.ModeFullAuto},
		{name: "session effort only", effort: "low", mode: "ask", session: true, want: connect.ModeFullAuto, wantMode: connect.ModeAsk},
		{name: "model without session", model: "opus", exit: ExitUsageError, msg: "--model needs --session"},
		{name: "effort without session", effort: "high", exit: ExitUsageError, msg: "--effort needs --session"},
		{name: "bad model", model: "-opus", session: true, exit: ExitUsageError, msg: "--model \"-opus\" is not a model name"},
		{name: "model with a space", model: "claude opus", session: true, exit: ExitUsageError, msg: "is not a model name"},
		{name: "bad effort", effort: "High", session: true, exit: ExitUsageError, msg: "--effort \"High\" is not an effort name"},
	} {
		var st *connect.SessionStart
		if c.session {
			st = sess()
		}
		var stderr bytes.Buffer
		got, err := permissionFlags(&stderr, c.max, sessionControlFlags{mode: c.mode, model: c.model, effort: c.effort}, st)
		if exitOf(err) != c.exit || (c.exit == 0 && got != c.want) || !strings.Contains(stderr.String(), c.msg) {
			t.Errorf("%s: %q exit %d stderr %q", c.name, got, exitOf(err), stderr.String())
		}
		if st != nil && c.exit == 0 && (st.Mode != c.wantMode || st.Model != c.model || st.Effort != c.effort) {
			t.Errorf("%s: session start %+v, want mode %q model %q effort %q", c.name, *st, c.wantMode, c.model, c.effort)
		}
	}
}

func TestConnectHelpListsModes(t *testing.T) {
	c := newConnectCmd("dev")
	f := c.Flags().Lookup("max-permission")
	if f == nil || !strings.Contains(f.Usage, connect.ModeHelp()) {
		t.Fatalf("--max-permission help %v", f)
	}
	if c.Flags().Lookup("mode") == nil || !strings.Contains(c.Long, "Mosaic search and read tools never ask") {
		t.Fatal("--mode flag or long help missing")
	}
	for _, name := range []string{"model", "effort"} {
		if f := c.Flags().Lookup(name); f == nil || !strings.Contains(f.Usage, "With --session") {
			t.Fatalf("--%s flag %v", name, f)
		}
	}
}
