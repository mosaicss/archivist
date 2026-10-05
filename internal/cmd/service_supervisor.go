package cmd

import (
	"context"
	"strings"
	"time"

	"github.com/mosaicss/archivist/internal/connect"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// Story 78.34: on Windows the scheduled task runs `archivistw.exe connect
// --service`, which supervises the daemon instead of being it: Task
// Scheduler restarts a task whose launch failed, not one that exited
// non-zero, so the supervisor gives 78.30's exit mapping exactly
// (Restart=on-failure, RestartSec=10): an unexpected exit restarts the
// daemon after 10 s, a terminal outcome (exit 0) stops the supervisor too.

// supervisorRestartDelay mirrors systemd RestartSec=10 and launchd
// ThrottleInterval 10. A var so tests can shorten it.
var supervisorRestartDelay = 10 * time.Second

// superviseDaemon starts the daemon through start and waits for it: a nil
// wait (exit 0, a terminal outcome) ends supervision with nil; an error
// (non-zero exit, or a failed start) restarts it after
// supervisorRestartDelay. It returns nil when ctx ends while it waits.
func superviseDaemon(ctx context.Context, log *connect.Logger, start func() (wait func() error, err error)) error {
	for {
		wait, err := start()
		if err != nil {
			log.Printf("supervisor: could not start the daemon: %v; retrying in %s", err, supervisorRestartDelay)
		} else {
			if err = wait(); err == nil {
				log.Printf("supervisor: the daemon stopped (exit 0); it stays stopped until 'archivist connect --install' or the next login")
				return nil
			}
			log.Printf("supervisor: the daemon exited unexpectedly (%v); it restarts in %s", err, supervisorRestartDelay)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(supervisorRestartDelay):
		}
	}
}

// serviceChildArgs are the daemon child's arguments: connect --service and
// every other flag given on this command line (the task passes
// --max-permission; a hand run may add harness flags such as
// --claude-model or --web-search), in the form that keeps a flag
// explicitly set, never --token (the service reads the saved key only).
func serviceChildArgs(cmd *cobra.Command) []string {
	args := []string{"connect", "--service"}
	cmd.Flags().Visit(func(f *pflag.Flag) {
		switch {
		case f.Name == "service" || f.Name == "token":
		case f.Value.Type() == "bool" || strings.HasPrefix(f.Value.String(), "-"):
			args = append(args, "--"+f.Name+"="+f.Value.String())
		default:
			args = append(args, "--"+f.Name, f.Value.String())
		}
	})
	return args
}
