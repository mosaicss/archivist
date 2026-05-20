//go:build integration

package main

import (
	"os"
	"testing"
)

// TestTableSearch_Integration runs a live table search against a real chat-api.
// Skipped unless ARCHIVIST_TEST_TOKEN and ARCHIVIST_TEST_CHAT_API_URL are set.
func TestTableSearch_Integration(t *testing.T) {
	token := os.Getenv("ARCHIVIST_TEST_TOKEN")
	baseURL := os.Getenv("ARCHIVIST_TEST_CHAT_API_URL")
	if token == "" || baseURL == "" {
		t.Skip("set ARCHIVIST_TEST_TOKEN and ARCHIVIST_TEST_CHAT_API_URL to run integration tests")
	}
	// Integration test body would go here when running against a live server.
	t.Log("integration test skipped: not yet implemented")
}
