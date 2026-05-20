// Package client holds the HTTP client that talks to the Mosaic chat-api.
//
// Filled in by Story 36.2 + 36.3: typed wrappers for /chat, /table/*,
// /companies/search, /companies/get, /account/cli-tokens. Bearer auth from
// internal/auth, structured retry policy (3 attempts, 250/500/1000ms +/- 25%
// jitter, honors Retry-After on 429), X-Archivist-CLI-Version header on every
// request, X-Archivist-Origin pass-through, X-Archivist-Min-CLI-Version
// response handling (exit 5 when server says CLI too old).
package client
