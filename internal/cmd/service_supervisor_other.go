//go:build !windows

package cmd

import (
	"context"
	"io"
	"os"

	"github.com/mosaicss/archivist/internal/connect"
)

// serviceBinary is the binary the service definition runs: the stable
// archivist itself outside Windows.
func serviceBinary(stable string) string { return stable }

// checkServiceBinary has nothing to check outside Windows.
func checkServiceBinary(io.Writer) error { return nil }

// serviceSupervisorNeeded is false: launchd and systemd supervise the
// daemon themselves.
func serviceSupervisorNeeded() bool { return false }

// runServiceSupervisor is never reached outside Windows.
func runServiceSupervisor(context.Context, *connect.Logger, string, []string) error { return nil }

// setServiceCrashOutput leaves fatal output on stderr, which launchd and
// systemd already send to the log or the journal.
func setServiceCrashOutput(*os.File) func() { return func() {} }
