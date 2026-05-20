package doctor

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/mosaicss/archivist/internal/auth"
	"github.com/mosaicss/archivist/internal/client"
)

// Status represents the outcome of a single health check.
type Status int

const (
	StatusPass Status = iota
	StatusWarn
	StatusFail
	StatusSkip
)

func (s Status) String() string {
	switch s {
	case StatusPass:
		return "ok"
	case StatusWarn:
		return "warn"
	case StatusFail:
		return "fail"
	case StatusSkip:
		return "skip"
	default:
		return "unknown"
	}
}

// CheckResult holds the outcome of a single doctor check.
type CheckResult struct {
	Name       string
	Status     Status
	Detail     string
	Message    string
	Suggestion string
}

// RunConfig holds injectable dependencies for all checks.
type RunConfig struct {
	Token      string
	Version    string
	Commit     string
	Date       string
	HTTPClient *http.Client
	BaseURL    string
	NoNetwork  bool
	// SkillPath overrides the default ~/.claude/skills/archivist/SKILL.md path (for tests).
	SkillPath string
}

// Check1Binary always passes — reports build info.
func Check1Binary(cfg *RunConfig) CheckResult {
	detail := fmt.Sprintf("archivist-cli %s (commit %s built %s) %s/%s",
		cfg.Version, cfg.Commit, cfg.Date, runtime.GOOS, runtime.GOARCH)
	return CheckResult{Name: "Binary", Status: StatusPass, Detail: detail}
}

// Check2Skill checks for ~/.claude/skills/archivist/SKILL.md and reads the version frontmatter.
func Check2Skill(cfg *RunConfig) CheckResult {
	path := cfg.SkillPath
	if path == "" {
		if envPath := os.Getenv("ARCHIVIST_SKILL_PATH"); envPath != "" {
			path = envPath
		} else {
			home, err := os.UserHomeDir()
			if err != nil {
				return CheckResult{Name: "Skill", Status: StatusWarn, Message: "could not determine home directory"}
			}
			path = home + "/.claude/skills/archivist/SKILL.md"
		}
	}
	f, err := os.Open(path)
	if err != nil {
		return CheckResult{
			Name:       "Skill",
			Status:     StatusWarn,
			Detail:     path,
			Message:    "skill not installed; run installer to get /archivist skill",
			Suggestion: "Re-run the archivist skill installer from the dashboard",
		}
	}
	defer func() { _ = f.Close() }()

	skillVersion := parseSkillVersion(f)
	if skillVersion == "" {
		return CheckResult{
			Name:    "Skill",
			Status:  StatusWarn,
			Detail:  path,
			Message: "SKILL.md found but version field is missing or unreadable",
		}
	}
	if skillVersion != cfg.Version && cfg.Version != "dev" {
		return CheckResult{
			Name:       "Skill",
			Status:     StatusWarn,
			Detail:     fmt.Sprintf("%s v%s", path, skillVersion),
			Message:    fmt.Sprintf("skill v%s does not match binary v%s; run `archivist update`", skillVersion, cfg.Version),
			Suggestion: "Run `archivist update` to sync skill and binary versions",
		}
	}
	return CheckResult{
		Name:   "Skill",
		Status: StatusPass,
		Detail: fmt.Sprintf("%s v%s", path, skillVersion),
	}
}

// parseSkillVersion reads the YAML frontmatter version field from a SKILL.md reader.
func parseSkillVersion(r io.Reader) string {
	scanner := bufio.NewScanner(r)
	inFrontmatter := false
	for scanner.Scan() {
		line := scanner.Text()
		if line == "---" {
			if !inFrontmatter {
				inFrontmatter = true
				continue
			}
			break
		}
		if inFrontmatter && strings.HasPrefix(line, "version:") {
			parts := strings.SplitN(line, ":", 2)
			if len(parts) == 2 {
				return strings.TrimSpace(parts[1])
			}
		}
	}
	return ""
}

// Check3Credentials checks that ARCHIVIST_TOKEN is set.
func Check3Credentials(cfg *RunConfig) CheckResult {
	if cfg.Token == "" {
		return CheckResult{
			Name:       "Credentials",
			Status:     StatusFail,
			Message:    "ARCHIVIST_TOKEN not set",
			Suggestion: "Visit https://mosaic-finance.com/account/cli-tokens to issue a token",
		}
	}
	return CheckResult{
		Name:   "Credentials",
		Status: StatusPass,
		Detail: fmt.Sprintf("ARCHIVIST_TOKEN set (key_id: %s, fingerprint: %s)", tokenKeyID(cfg.Token), tokenFingerprint(cfg.Token)),
	}
}

// Check4Token validates the token format and derives display fields.
func Check4Token(cfg *RunConfig) CheckResult {
	if cfg.Token == "" {
		return CheckResult{Name: "Token", Status: StatusSkip, Detail: "skipped: no credential"}
	}
	if err := auth.ValidateTokenFormat(cfg.Token); err != nil {
		return CheckResult{
			Name:       "Token",
			Status:     StatusFail,
			Message:    "token format unrecognized; expected ak_... prefix",
			Suggestion: "Create a new key via the avatar menu → Manage account → API keys on https://mosaic-finance.com",
		}
	}
	return CheckResult{
		Name:   "Token",
		Status: StatusPass,
		Detail: fmt.Sprintf("key_id: %s, fingerprint: %s", tokenKeyID(cfg.Token), tokenFingerprint(cfg.Token)),
	}
}

// ServerProbeResult holds data from the server probe used by checks 5, 6, 8.
type ServerProbeResult struct {
	Reachable  bool
	StatusCode int
	RTTms      int64
	MinVersion string
	Error      string
	RespHeader http.Header
}

// Check5Server probes the unauthenticated /health endpoint.
func Check5Server(ctx context.Context, cfg *RunConfig) (CheckResult, *ServerProbeResult) {
	baseURL := cfg.BaseURL
	if baseURL == "" {
		baseURL = client.DefaultBaseURL
	}
	url := baseURL + "/health"

	start := time.Now()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		probe := &ServerProbeResult{Error: err.Error()}
		return CheckResult{
			Name:       "Server",
			Status:     StatusFail,
			Message:    fmt.Sprintf("server unreachable (%s); check network or VPN", baseURL),
			Suggestion: "Check your network connection or VPN",
		}, probe
	}

	httpC := cfg.HTTPClient
	if httpC == nil {
		httpC = &http.Client{Timeout: 5 * time.Second}
	}

	resp, err := httpC.Do(req)
	rtt := time.Since(start).Milliseconds()
	if err != nil {
		probe := &ServerProbeResult{Error: err.Error()}
		return CheckResult{
			Name:       "Server",
			Status:     StatusFail,
			Message:    fmt.Sprintf("server unreachable (%s); check network or VPN", baseURL),
			Suggestion: "Check your network connection or VPN",
		}, probe
	}
	defer func() { _ = resp.Body.Close() }()

	probe := &ServerProbeResult{
		Reachable:  resp.StatusCode < 500,
		StatusCode: resp.StatusCode,
		RTTms:      rtt,
		MinVersion: resp.Header.Get("X-Archivist-Min-CLI-Version"),
		RespHeader: resp.Header,
	}

	if resp.StatusCode >= 500 {
		probe.Error = fmt.Sprintf("server returned %d", resp.StatusCode)
		return CheckResult{
			Name:    "Server",
			Status:  StatusFail,
			Message: fmt.Sprintf("server returned %d; chat-api may be degraded", resp.StatusCode),
		}, probe
	}

	return CheckResult{
		Name:   "Server",
		Status: StatusPass,
		Detail: fmt.Sprintf("chat-api reachable (%dms RTT)", rtt),
	}, probe
}

// Check6MinVersion checks the X-Archivist-Min-CLI-Version header from the probe.
func Check6MinVersion(cfg *RunConfig, probe *ServerProbeResult) CheckResult {
	if probe == nil || !probe.Reachable {
		return CheckResult{Name: "MinVersion", Status: StatusSkip, Detail: "skipped: server unreachable"}
	}
	minVer := probe.MinVersion
	if minVer == "" {
		return CheckResult{Name: "MinVersion", Status: StatusPass, Detail: "no minimum version required"}
	}
	if isOlderVersion(cfg.Version, minVer) {
		return CheckResult{
			Name:       "MinVersion",
			Status:     StatusFail,
			Detail:     fmt.Sprintf("required: %s, have: %s", minVer, cfg.Version),
			Message:    fmt.Sprintf("server requires archivist-cli >= %s; you have %s; run `archivist update`", minVer, cfg.Version),
			Suggestion: "Run `archivist update` to upgrade",
		}
	}
	return CheckResult{
		Name:   "MinVersion",
		Status: StatusPass,
		Detail: fmt.Sprintf("binary %s >= required %s", cfg.Version, minVer),
	}
}

// CLITokensInfo holds parsed data from GET /account/cli-tokens.
type CLITokensInfo struct {
	UserEmail string
	Tier      string
	CLIScope  string
}

// Check7User calls GET /account/cli-tokens to validate the token and get user info.
func Check7User(ctx context.Context, cfg *RunConfig, probe *ServerProbeResult) (CheckResult, *CLITokensInfo) {
	if probe != nil && !probe.Reachable {
		return CheckResult{Name: "User", Status: StatusSkip, Detail: "skipped: server unreachable"}, nil
	}
	if cfg.Token == "" {
		return CheckResult{Name: "User", Status: StatusSkip, Detail: "skipped: no credential"}, nil
	}

	baseURL := cfg.BaseURL
	if baseURL == "" {
		baseURL = client.DefaultBaseURL
	}
	url := baseURL + "/account/cli-tokens"

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return CheckResult{
			Name:    "User",
			Status:  StatusFail,
			Message: fmt.Sprintf("request build error: %v", err),
		}, nil
	}
	req.Header.Set("Authorization", "Bearer "+cfg.Token)
	req.Header.Set("X-Archivist-CLI-Version", cfg.Version)

	httpC := cfg.HTTPClient
	if httpC == nil {
		httpC = &http.Client{Timeout: 10 * time.Second}
	}

	resp, err := httpC.Do(req)
	if err != nil {
		return CheckResult{Name: "User", Status: StatusSkip, Detail: "skipped: server unreachable"}, nil
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusUnauthorized {
		return CheckResult{
			Name:       "User",
			Status:     StatusFail,
			Message:    "token invalid or revoked; re-issue at https://mosaic-finance.com/account/cli-tokens",
			Suggestion: "Re-issue your token at https://mosaic-finance.com/account/cli-tokens",
		}, nil
	}
	if resp.StatusCode == http.StatusForbidden {
		return CheckResult{
			Name:       "User",
			Status:     StatusFail,
			Message:    "token lacks CLI scope; this account does not have an active Archivist CLI subscription",
			Suggestion: "Subscribe to Archivist CLI at https://mosaic-finance.com/account",
		}, nil
	}
	if resp.StatusCode != http.StatusOK {
		return CheckResult{
			Name:    "User",
			Status:  StatusFail,
			Message: fmt.Sprintf("unexpected server response: %d", resp.StatusCode),
		}, nil
	}

	var body struct {
		UserEmail string `json:"user_email"`
		Tier      string `json:"tier"`
		Tokens    []struct {
			KeyID string `json:"key_id"`
		} `json:"tokens"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return CheckResult{
			Name:    "User",
			Status:  StatusFail,
			Message: fmt.Sprintf("response parse error: %v", err),
		}, nil
	}

	cliScope := "active"
	statusResult := StatusPass
	var warnMsg string
	if body.Tier == "free" {
		statusResult = StatusWarn
		cliScope = "none"
		warnMsg = "tier is free; CLI scope requires an Archivist CLI subscription"
	}

	info := &CLITokensInfo{
		UserEmail: body.UserEmail,
		Tier:      body.Tier,
		CLIScope:  cliScope,
	}

	detail := fmt.Sprintf("email: %s, tier: %s, cli_scope: %s", body.UserEmail, body.Tier, cliScope)
	return CheckResult{
		Name:    "User",
		Status:  statusResult,
		Detail:  detail,
		Message: warnMsg,
	}, info
}

// Check8Quota reads X-Queries-Remaining from the user check's response or probe headers.
func Check8Quota(cfg *RunConfig, userResp *CLITokensInfo, probe *ServerProbeResult) CheckResult {
	if probe != nil && !probe.Reachable {
		return CheckResult{Name: "Quota", Status: StatusSkip, Detail: "skipped: server unreachable"}
	}
	if cfg.Token == "" {
		return CheckResult{Name: "Quota", Status: StatusSkip, Detail: "skipped: no credential"}
	}

	// Read from probe headers (check 5's /health response) or user check headers
	var remaining string
	if probe != nil && probe.RespHeader != nil {
		remaining = probe.RespHeader.Get("X-Queries-Remaining")
	}

	if remaining == "" {
		return CheckResult{
			Name:    "Quota",
			Status:  StatusWarn,
			Detail:  "quota header not returned",
			Message: "quota header not returned; cannot determine remaining quota",
		}
	}

	var n int
	if _, err := fmt.Sscanf(remaining, "%d", &n); err != nil {
		return CheckResult{
			Name:    "Quota",
			Status:  StatusWarn,
			Detail:  remaining,
			Message: fmt.Sprintf("quota header value %q is not a valid integer", remaining),
		}
	}

	if n == 0 {
		return CheckResult{
			Name:    "Quota",
			Status:  StatusWarn,
			Detail:  "0 queries remaining",
			Message: "quota exhausted; table/chat queries will be blocked until next billing cycle",
		}
	}
	if n <= 5 {
		return CheckResult{
			Name:    "Quota",
			Status:  StatusWarn,
			Detail:  fmt.Sprintf("%d queries remaining", n),
			Message: fmt.Sprintf("quota low: %d queries remaining", n),
		}
	}
	return CheckResult{
		Name:   "Quota",
		Status: StatusPass,
		Detail: fmt.Sprintf("%d queries remaining", n),
	}
}

// ResolveExitCode determines the exit code from a set of results.
func ResolveExitCode(results []CheckResult) int {
	hasAuthFail, hasServerFail, hasOtherFail := false, false, false
	for _, r := range results {
		if r.Status != StatusFail {
			continue
		}
		switch r.Name {
		case "Credentials", "Token", "User":
			hasAuthFail = true
		case "Server", "MinVersion":
			hasServerFail = true
		default:
			hasOtherFail = true
		}
	}
	if hasAuthFail {
		return 4
	}
	if hasServerFail {
		return 5
	}
	if hasOtherFail {
		return 1
	}
	return 0
}

// tokenFingerprint derives SHA256(key_id)[:8] hex from the token.
// Per OQ4 resolution (2026-05-20): fingerprint is SHA256 of key_id, not raw token.
func tokenFingerprint(token string) string {
	keyID := extractKeyID(token)
	h := sha256.Sum256([]byte(keyID))
	return fmt.Sprintf("%x", h[:4])
}

// tokenKeyID derives a display-safe key_id string from the token.
// Shows first 10 chars and last 3 chars; never the full token.
func tokenKeyID(token string) string {
	if len(token) < 15 {
		return "ak_???"
	}
	return token[:10] + "..." + token[len(token)-3:]
}

// extractKeyID derives a key_id component for fingerprinting display:
// "<prefix>_<first-8-chars-after-prefix>". Falls back to first 10 chars
// if no recognized prefix.
func extractKeyID(token string) string {
	for _, prefix := range []string{"ak_", "mc_pat_"} {
		if !strings.HasPrefix(token, prefix) {
			continue
		}
		rest := token[len(prefix):]
		if len(rest) >= 8 {
			return prefix + rest[:8]
		}
		return token
	}
	if len(token) >= 10 {
		return token[:10]
	}
	return token
}

// isOlderVersion returns true if current < minimum (semver).
func isOlderVersion(current, minimum string) bool {
	cur := parseSemver(current)
	min := parseSemver(minimum)
	if cur == nil || min == nil {
		return false
	}
	for i := range cur {
		if cur[i] < min[i] {
			return true
		}
		if cur[i] > min[i] {
			return false
		}
	}
	return false
}

// parseSemver parses vX.Y.Z or X.Y.Z into a [3]int slice.
func parseSemver(v string) []int {
	if len(v) > 0 && v[0] == 'v' {
		v = v[1:]
	}
	var major, minor, patch int
	_, err := fmt.Sscanf(v, "%d.%d.%d", &major, &minor, &patch)
	if err != nil {
		return nil
	}
	return []int{major, minor, patch}
}
