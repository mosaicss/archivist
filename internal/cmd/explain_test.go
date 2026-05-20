package cmd_test

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/mosaicss/archivist/internal/cmd"
)

func TestExplainDefaults_TextFormat(t *testing.T) {
	root := cmd.NewRootCmd("test", "abc1234", "2026-05-20")
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"explain", "defaults"})

	if err := root.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	result := out.String()
	for _, section := range []string{
		"DEFAULT DATE WINDOW",
		"RECOMMENDED WINDOWS",
		"HARD CAP",
		"CASCADE PATTERN",
	} {
		if !strings.Contains(result, section) {
			t.Errorf("output missing section %q", section)
		}
	}
}

func TestExplainDefaults_JSON(t *testing.T) {
	root := cmd.NewRootCmd("test", "abc1234", "2026-05-20")
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"explain", "defaults", "--format", "json"})

	if err := root.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var sections map[string]interface{}
	if err := json.Unmarshal(out.Bytes(), &sections); err != nil {
		t.Fatalf("output is not valid JSON: %v\noutput: %s", err, out.String())
	}

	for _, key := range []string{"default_date_window", "recommended_windows", "hard_cap", "cascade_pattern"} {
		if _, ok := sections[key]; !ok {
			t.Errorf("JSON output missing key %q", key)
		}
	}
}

func TestExplainDefaults_NoEmDashes(t *testing.T) {
	root := cmd.NewRootCmd("test", "abc1234", "2026-05-20")
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"explain", "defaults"})

	if err := root.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	result := out.String()
	// Em dash U+2014
	if strings.ContainsRune(result, '—') {
		t.Error("output contains an em dash (\\u2014); not allowed in user-facing copy")
	}
	// En dash U+2013
	if strings.ContainsRune(result, '–') {
		t.Error("output contains an en dash (\\u2013); not allowed in user-facing copy")
	}
}
