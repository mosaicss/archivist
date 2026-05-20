// Package auth holds the credential loading + validation logic that the
// archivist binary uses to authenticate against the Mosaic chat-api.
//
// Filled in by Story 36.2: ARCHIVIST_TOKEN env var read, dashboard-issued
// Clerk API keys, `archivist auth status` validation against
// GET /account/cli-tokens, per-call --token flag handling.
package auth
