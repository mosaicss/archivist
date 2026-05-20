package flags_test

import (
	"testing"

	"github.com/mosaicss/archivist/internal/cmd/flags"
	"github.com/spf13/cobra"
)

func TestAgentFlagsRegistered(t *testing.T) {
	cmd := &cobra.Command{Use: "test"}
	flags.RegisterAgentFlags(cmd)

	expected := []string{"compact", "dry-run", "quiet", "no-color", "stdin"}
	for _, name := range expected {
		if f := cmd.Flags().Lookup(name); f == nil {
			t.Errorf("flag --%s not registered", name)
		}
	}
}

func TestReadAgentFlagsDefaults(t *testing.T) {
	cmd := &cobra.Command{Use: "test"}
	flags.RegisterAgentFlags(cmd)

	got := flags.ReadAgentFlags(cmd)
	if got.Compact || got.DryRun || got.Quiet || got.NoColor || got.Stdin {
		t.Errorf("expected all false defaults, got %+v", got)
	}
}

func TestReadAgentFlagsSet(t *testing.T) {
	cmd := &cobra.Command{Use: "test"}
	flags.RegisterAgentFlags(cmd)
	_ = cmd.ParseFlags([]string{"--compact", "--dry-run", "--quiet", "--no-color", "--stdin"})

	got := flags.ReadAgentFlags(cmd)
	if !got.Compact {
		t.Error("expected Compact=true")
	}
	if !got.DryRun {
		t.Error("expected DryRun=true")
	}
	if !got.Quiet {
		t.Error("expected Quiet=true")
	}
	if !got.NoColor {
		t.Error("expected NoColor=true")
	}
	if !got.Stdin {
		t.Error("expected Stdin=true")
	}
}
