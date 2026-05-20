// Package cmd hosts the Cobra command tree for the archivist binary.
package cmd

// Exit codes emitted by the archivist binary. Agents that orchestrate
// archivist read these to self-correct without LLM-driven retry loops.
//
//	0 = success
//	1 = generic error
//	2 = usage error (bad flag combo, bad spec)
//	3 = not found (companies get <id> matched nothing; missing file; invalid session_id)
//	4 = auth error (missing/invalid/expired credential; tier mismatch)
//	5 = server error (5xx after retries; X-Archivist-Min-CLI-Version block)
//	6 = ambiguous match (multiple candidates; rerun with --company <id>)
//	7 = rate limit (429 after retries; per-user quota exhausted)
//	8 = cascade violation (custom-entity x filings column; country lock conflict)
//	9 = not implemented (stub verb in current binary; do NOT retry the verb)
//
// Reference: architecture E36 §11.4 (audit revision 2026-05-19).
const (
	ExitOK               = 0
	ExitGenericError     = 1
	ExitUsageError       = 2
	ExitNotFound         = 3
	ExitAuthError        = 4
	ExitServerError      = 5
	ExitAmbiguousMatch   = 6
	ExitRateLimit        = 7
	ExitCascadeViolation = 8
	ExitNotImplemented   = 9
)
