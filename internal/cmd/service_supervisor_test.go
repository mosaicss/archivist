package cmd

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/mosaicss/archivist/internal/connect"
)

func testLogger() *connect.Logger { return connect.NewLogger(io.Discard) }

// The supervisor restarts the daemon after an unexpected exit or a failed
// start and stops on exit 0 (Restart=on-failure semantics); a cancelled
// context ends it while it waits.
func TestSuperviseDaemonRestartAndStop(t *testing.T) {
	prev := supervisorRestartDelay
	supervisorRestartDelay = time.Millisecond
	t.Cleanup(func() { supervisorRestartDelay = prev })
	outcomes := []error{errors.New("exit status 1"), nil}
	starts, failedStart := 0, false
	err := superviseDaemon(context.Background(), testLogger(), func() (func() error, error) {
		starts++
		if !failedStart {
			failedStart = true
			return nil, errors.New("cannot start")
		}
		out := outcomes[0]
		outcomes = outcomes[1:]
		return func() error { return out }, nil
	})
	if err != nil || starts != 3 || len(outcomes) != 0 {
		t.Fatalf("err %v starts %d left %d", err, starts, len(outcomes))
	}

	supervisorRestartDelay = time.Hour
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- superviseDaemon(ctx, testLogger(), func() (func() error, error) {
			return func() error { return errors.New("crash") }, nil
		})
	}()
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("cancelled supervisor: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the supervisor did not stop on cancel")
	}
}

// The daemon child gets connect --service and every flag this command line
// set (bools in the --name=value form that keeps them explicitly set),
// never --token.
func TestServiceChildArgs(t *testing.T) {
	root := NewRootCmd("v0", "c", "d")
	argv := []string{"connect", "--service", "--max-permission", "ask", "--web-search=false", "--claude-model", "m", "--token", "ak_x"}
	c, rest, err := root.Find(argv)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.ParseFlags(rest); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(serviceChildArgs(c), " ")
	if want := "connect --service --claude-model m --max-permission ask --web-search=false"; got != want {
		t.Fatalf("child args %q, want %q", got, want)
	}
	// A value starting with "-" keeps the --name=value form, so the child
	// never reads it as a flag.
	root = NewRootCmd("v0", "c", "d")
	c, rest, err = root.Find([]string{"connect", "--service", "--claude-model=-x"})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.ParseFlags(rest); err != nil {
		t.Fatal(err)
	}
	args := serviceChildArgs(c)
	if strings.Join(args, "|") != "connect|--service|--claude-model=-x" {
		t.Fatalf("child args %q", args)
	}
	child, rest, err := NewRootCmd("v0", "c", "d").Find(args)
	if err != nil {
		t.Fatal(err)
	}
	if err := child.ParseFlags(rest); err != nil {
		t.Fatal(err)
	}
	if v, _ := child.Flags().GetString("claude-model"); v != "-x" {
		t.Fatalf("the child reads --claude-model %q", v)
	}
}
