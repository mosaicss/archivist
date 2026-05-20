package cmd

import (
	"bytes"
	"errors"
	"fmt"
	"regexp"
	"runtime"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func TestVersionCommand(t *testing.T) {
	root := NewRootCmd("0.1.0-test", "abc1234", "2026-05-19")
	root.SetArgs([]string{"version"})
	out := &bytes.Buffer{}
	root.SetOut(out)
	root.SetErr(&bytes.Buffer{})
	if err := root.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	want := fmt.Sprintf("archivist-cli 0.1.0-test (commit abc1234 built 2026-05-19) %s/%s\n",
		runtime.GOOS, runtime.GOARCH)
	if got := out.String(); got != want {
		t.Errorf("version output mismatch\n got: %q\nwant: %q", got, want)
	}
}

func TestVersionCommandPattern(t *testing.T) {
	root := NewRootCmd("dev", "unknown", "unknown")
	root.SetArgs([]string{"version"})
	out := &bytes.Buffer{}
	root.SetOut(out)
	root.SetErr(&bytes.Buffer{})
	if err := root.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	pattern := regexp.MustCompile(`^archivist-cli \S+ \(commit \S+ built \S+\) \S+/\S+\n$`)
	if !pattern.MatchString(out.String()) {
		t.Errorf("version output %q did not match pattern %q", out.String(), pattern)
	}
}

func TestHelpListsAllVerbsInOrder(t *testing.T) {
	root := NewRootCmd("dev", "unknown", "unknown")
	root.SetArgs([]string{"--help"})
	out := &bytes.Buffer{}
	root.SetOut(out)
	root.SetErr(&bytes.Buffer{})
	if err := root.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	helpText := out.String()
	verbsInOrder := []string{"auth", "chat", "table", "companies", "usage", "update", "version"}
	lastIdx := -1
	for _, verb := range verbsInOrder {
		// Cobra renders subcommands as "  <use>   <short>" under
		// "Available Commands:". Match the verb at the start of an indented
		// line followed by at least one space.
		re := regexp.MustCompile(`(?m)^\s{2,}` + regexp.QuoteMeta(verb) + `(\s|$)`)
		loc := re.FindStringIndex(helpText)
		if loc == nil {
			t.Errorf("verb %q not found in help output:\n%s", verb, helpText)
			continue
		}
		if loc[0] <= lastIdx {
			t.Errorf("verb %q appeared at byte %d but previous verb was at %d (expected order: %v)\nhelp:\n%s",
				verb, loc[0], lastIdx, verbsInOrder, helpText)
		}
		lastIdx = loc[0]
	}
}

func TestStubVerbReturnsNotImplemented(t *testing.T) {
	cases := []struct {
		verb, story string
	}{
		// auth is no longer a stub — it's a real command (Story 36.2)
		{"chat", "36.3"},
		{"table", "36.4"},
		{"companies", "36.5"},
		{"usage", "36.12"},
		{"update", "36.11"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.verb, func(t *testing.T) {
			root := NewRootCmd("dev", "unknown", "unknown")
			root.SetArgs([]string{tc.verb})
			stderr := &bytes.Buffer{}
			root.SetErr(stderr)
			root.SetOut(&bytes.Buffer{})
			err := root.Execute()
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			var exitErr *ExitError
			if !errors.As(err, &exitErr) {
				t.Fatalf("expected *ExitError, got %T (%v)", err, err)
			}
			if exitErr.Code != ExitNotImplemented {
				t.Errorf("expected exit code %d (ExitNotImplemented), got %d", ExitNotImplemented, exitErr.Code)
			}
			wantLine := fmt.Sprintf("archivist %s: not yet implemented (lands in Story %s)\n",
				tc.verb, tc.story)
			if got := stderr.String(); !strings.Contains(got, wantLine) {
				t.Errorf("stderr did not contain expected line\n got: %q\nwant substring: %q",
					got, wantLine)
			}
		})
	}
}

func TestAllCommandsHaveAnnotations(t *testing.T) {
	root := NewRootCmd("dev", "unknown", "unknown")
	walkAndAssertAnnotations(t, root)
}

func walkAndAssertAnnotations(t *testing.T, c *cobra.Command) {
	t.Helper()
	// Skip auto-generated cobra commands not registered by this story.
	if c.Name() == "help" || c.Name() == "completion" {
		return
	}
	if _, ok := c.Annotations["pp:typed-exit-codes"]; !ok {
		t.Errorf("command %q missing pp:typed-exit-codes annotation", c.CommandPath())
	}
	for _, child := range c.Commands() {
		walkAndAssertAnnotations(t, child)
	}
}

// TestStubVerbExitErrorImplementsError is a smoke check that the ExitError
// type satisfies the error interface (defensive — easy to miss on refactor).
func TestExitErrorImplementsErrorInterface(t *testing.T) {
	var _ error = &ExitError{Code: ExitNotImplemented}
}
