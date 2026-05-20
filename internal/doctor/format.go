package doctor

import (
	"encoding/json"
	"fmt"
	"io"
)

// PrintHuman writes the human-readable doctor report to w.
// If quiet is true, only FAIL and WARN lines are printed (not passing checks).
// noNetwork indicates whether --no-network was passed.
func PrintHuman(w io.Writer, results []CheckResult, noNetwork bool, quiet bool) {
	if !quiet {
		_, _ = fmt.Fprintln(w, "ARCHIVIST DOCTOR")
		_, _ = fmt.Fprintln(w, "================")
	}

	failCount := 0
	for _, r := range results {
		if r.Status == StatusFail {
			failCount++
		}
	}

	for _, r := range results {
		line := formatLine(r)
		if quiet && r.Status == StatusPass {
			continue
		}
		_, _ = fmt.Fprintln(w, line)
	}

	if noNetwork {
		_, _ = fmt.Fprintln(w, "(server checks skipped: --no-network)")
	}

	if !quiet {
		if failCount == 0 {
			_, _ = fmt.Fprintln(w, "\nAll checks passed.")
		} else {
			_, _ = fmt.Fprintf(w, "\n%d check(s) failed -- see FAIL lines above.\n", failCount)
		}
	}
}

func formatLine(r CheckResult) string {
	label := labelFor(r.Name)
	switch r.Status {
	case StatusPass:
		if r.Detail != "" {
			return fmt.Sprintf("%-15s %s", label, r.Detail)
		}
		return fmt.Sprintf("%-15s ok", label)
	case StatusWarn:
		msg := r.Message
		if msg == "" {
			msg = r.Detail
		}
		if r.Detail != "" && r.Detail != msg {
			return fmt.Sprintf("%-15s %s  [WARN: %s]", label, r.Detail, msg)
		}
		return fmt.Sprintf("%-15s [WARN: %s]", label, msg)
	case StatusFail:
		msg := r.Message
		if r.Detail != "" {
			return fmt.Sprintf("%-15s %s  [FAIL: %s]", label, r.Detail, msg)
		}
		return fmt.Sprintf("%-15s [FAIL: %s]", label, msg)
	case StatusSkip:
		return fmt.Sprintf("%-15s (skipped)", label)
	default:
		return fmt.Sprintf("%-15s ?", label)
	}
}

func labelFor(name string) string {
	switch name {
	case "Binary":
		return "Binary:"
	case "Skill":
		return "Skill:"
	case "Credentials":
		return "Credentials:"
	case "Token":
		return "Token:"
	case "Server":
		return "Server:"
	case "MinVersion":
		return "Min version:"
	case "User":
		return "User:"
	case "Quota":
		return "Quota:"
	default:
		return name + ":"
	}
}

// JSONReport is the full structured doctor output.
type JSONReport struct {
	Binary      BinaryJSON      `json:"binary"`
	Skill       SkillJSON       `json:"skill"`
	Credentials CredentialsJSON `json:"credentials"`
	User        UserJSON        `json:"user"`
	Server      ServerJSON      `json:"server"`
	MinVersion  MinVersionJSON  `json:"min_version"`
	Quota       QuotaJSON       `json:"quota"`
	Overall     OverallJSON     `json:"overall"`
}

type BinaryJSON struct {
	Version string `json:"version"`
	Commit  string `json:"commit"`
	Built   string `json:"built"`
	OS      string `json:"os"`
	Arch    string `json:"arch"`
}

type SkillJSON struct {
	Path    string `json:"path"`
	Version string `json:"version"`
	Status  string `json:"status"`
	Message string `json:"message,omitempty"`
}

type CredentialsJSON struct {
	Status      string `json:"status"`
	KeyID       string `json:"key_id,omitempty"`
	Fingerprint string `json:"fingerprint,omitempty"`
	Message     string `json:"message,omitempty"`
}

type UserJSON struct {
	Email    string `json:"email,omitempty"`
	Tier     string `json:"tier,omitempty"`
	CLIScope string `json:"cli_scope,omitempty"`
	Status   string `json:"status"`
	Message  string `json:"message,omitempty"`
}

type ServerJSON struct {
	Reachable bool   `json:"reachable"`
	RTTms     int64  `json:"rtt_ms,omitempty"`
	Status    string `json:"status"`
}

type MinVersionJSON struct {
	Required string `json:"required,omitempty"`
	Have     string `json:"have"`
	Status   string `json:"status"`
}

type QuotaJSON struct {
	Remaining int    `json:"remaining,omitempty"`
	Status    string `json:"status"`
}

type OverallJSON struct {
	Status   string `json:"status"`
	ExitCode int    `json:"exit_code"`
}

// BuildJSONReport assembles a JSONReport from the check results and config.
func BuildJSONReport(results []CheckResult, cfg *RunConfig, userInfo *CLITokensInfo, probe *ServerProbeResult) JSONReport {
	exitCode := ResolveExitCode(results)
	overall := "ok"
	if exitCode != 0 {
		overall = "fail"
	}

	report := JSONReport{
		Binary: BinaryJSON{
			Version: cfg.Version,
			Commit:  cfg.Commit,
			Built:   cfg.Date,
		},
		Overall: OverallJSON{
			Status:   overall,
			ExitCode: exitCode,
		},
	}

	// Populate per-check fields from results
	for _, r := range results {
		switch r.Name {
		case "Skill":
			report.Skill = SkillJSON{
				Status:  r.Status.String(),
				Message: r.Message,
			}
			if r.Detail != "" {
				report.Skill.Path = r.Detail
			}
		case "Credentials":
			report.Credentials = CredentialsJSON{
				Status:  r.Status.String(),
				Message: r.Message,
			}
			if cfg.Token != "" && r.Status != StatusFail {
				report.Credentials.KeyID = tokenKeyID(cfg.Token)
				report.Credentials.Fingerprint = tokenFingerprint(cfg.Token)
			}
		case "User":
			report.User = UserJSON{
				Status:  r.Status.String(),
				Message: r.Message,
			}
			if userInfo != nil {
				report.User.Email = userInfo.UserEmail
				report.User.Tier = userInfo.Tier
				report.User.CLIScope = userInfo.CLIScope
			}
		case "Server":
			report.Server = ServerJSON{
				Status: r.Status.String(),
			}
			if probe != nil {
				report.Server.Reachable = probe.Reachable
				report.Server.RTTms = probe.RTTms
			}
		case "MinVersion":
			report.MinVersion = MinVersionJSON{
				Have:   cfg.Version,
				Status: r.Status.String(),
			}
			if probe != nil {
				report.MinVersion.Required = probe.MinVersion
			}
		case "Quota":
			report.Quota = QuotaJSON{
				Status: r.Status.String(),
			}
		}
	}

	return report
}

// PrintJSON writes the JSONReport to w as indented JSON.
func PrintJSON(w io.Writer, report JSONReport) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(report)
}
