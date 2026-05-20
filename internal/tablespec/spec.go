// Package tablespec handles YAML/JSON spec file parsing for archivist table.
package tablespec

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"gopkg.in/yaml.v3"
)

// SpecRow represents a single row in a table spec.
// Custom and Company are mutually exclusive.
type SpecRow struct {
	// Company is an issuer_key or free-text company name.
	Company string `yaml:"company" json:"company,omitempty"`
	// Custom is a free-text custom entity (web-only row).
	Custom string `yaml:"custom" json:"custom,omitempty"`
	// FilingType may be a single string or a list (unmarshaled from YAML/JSON).
	FilingTypes []string `yaml:"-" json:"-"`
	// RawFilingType is the raw YAML value; parsed into FilingTypes by UnmarshalYAML.
	RawFilingType interface{} `yaml:"filing-type" json:"filing-type,omitempty"`
	DateFrom      string     `yaml:"date-from" json:"date-from,omitempty"`
	DateTo        string     `yaml:"date-to" json:"date-to,omitempty"`
	Exchange      string     `yaml:"exchange" json:"exchange,omitempty"`
	Sector        string     `yaml:"sector" json:"sector,omitempty"`
	Industry      string     `yaml:"industry" json:"industry,omitempty"`
}

// SpecColumn represents a single column in a table spec.
type SpecColumn struct {
	Name          string `yaml:"name" json:"name"`
	Source        string `yaml:"source" json:"source,omitempty"`           // "filings" | "web"
	Mode          string `yaml:"mode" json:"mode,omitempty"`               // rrf | vector | keyword
	Query         string `yaml:"query" json:"query,omitempty"`             // filings query
	SemanticQuery string `yaml:"semantic-query" json:"semantic-query,omitempty"`
	KeywordQuery  string `yaml:"keyword-query" json:"keyword-query,omitempty"`
	WebQuery      string `yaml:"web-query" json:"web-query,omitempty"`
}

// TableSpec mirrors the chat-api searchBodySchema (Zod schema in
// chat-api/src/routes/table.ts:159-164). Top-level spec file struct.
type TableSpec struct {
	TopN         int          `yaml:"top_n" json:"top_n"`
	Rows         []SpecRow    `yaml:"rows" json:"rows"`
	Columns      []SpecColumn `yaml:"columns" json:"columns"`
	SourceOffset *int         `yaml:"source_offset" json:"source_offset,omitempty"`
}

// Validate checks that the spec meets minimum constraints.
func (s *TableSpec) Validate() error {
	if s.TopN < 1 || s.TopN > 20 {
		return fmt.Errorf("top_n must be between 1 and 20 (got %d)", s.TopN)
	}
	if len(s.Rows) == 0 {
		return fmt.Errorf("rows: at least one row is required")
	}
	if len(s.Columns) == 0 {
		return fmt.Errorf("columns: at least one column is required")
	}
	for i, col := range s.Columns {
		if col.Name == "" {
			return fmt.Errorf("columns[%d]: name is required", i)
		}
		src := col.Source
		if src == "" {
			src = "filings"
		}
		switch src {
		case "filings":
			if col.Query == "" && col.SemanticQuery == "" && col.KeywordQuery == "" {
				return fmt.Errorf("columns[%d] (%s): query, semantic-query, or keyword-query is required for filings columns", i, col.Name)
			}
		case "web":
			if col.WebQuery == "" {
				return fmt.Errorf("columns[%d] (%s): web-query is required for web columns", i, col.Name)
			}
		default:
			return fmt.Errorf("columns[%d] (%s): source must be 'filings' or 'web', got %q", i, col.Name, col.Source)
		}
	}
	return nil
}

// maxYAMLNodes is the maximum number of YAML nodes allowed in a spec file.
const maxYAMLNodes = 10_000

// ParseYAML parses a YAML spec file with security hardening:
//   - Anchors rejected
//   - Merge keys (<<:) rejected
//   - Node count budget enforced
//   - Unknown keys cause parse failure
func ParseYAML(r io.Reader) (*TableSpec, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("read spec: %w", err)
	}

	// Pre-pass: parse into yaml.Node tree to check for anchors, merge keys,
	// and node count before full struct decode.
	var root yaml.Node
	if err := yaml.Unmarshal(data, &root); err != nil {
		return nil, fmt.Errorf("YAML parse error: %w", err)
	}

	nodeCount := 0
	if err := walkNodes(&root, func(n *yaml.Node) error {
		nodeCount++
		if nodeCount > maxYAMLNodes {
			return fmt.Errorf("spec file is too large (>10,000 nodes)")
		}
		if n.Anchor != "" {
			return fmt.Errorf("YAML anchors are not supported in spec files")
		}
		if n.Tag == "!!merge" {
			return fmt.Errorf("YAML merge keys (<<:) are not supported in spec files")
		}
		return nil
	}); err != nil {
		return nil, err
	}

	// Strict decode: unknown fields cause an error.
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	var spec TableSpec
	if err := dec.Decode(&spec); err != nil {
		// yaml.v3 unknown-field errors include "field <name> not found in type".
		// Reformat to make the key path clearer for the user.
		msg := err.Error()
		if strings.Contains(msg, "field") && strings.Contains(msg, "not found in type") {
			return nil, fmt.Errorf("unknown field in spec file: %s", msg)
		}
		return nil, fmt.Errorf("YAML decode error: %w", err)
	}

	// Normalize filing-type lists.
	for i := range spec.Rows {
		if err := normalizeFilingTypes(&spec.Rows[i]); err != nil {
			return nil, fmt.Errorf("rows[%d]: %w", i, err)
		}
	}

	return &spec, nil
}

// ParseJSON parses a JSON spec file with strict unknown-key checking.
func ParseJSON(r io.Reader) (*TableSpec, error) {
	dec := json.NewDecoder(r)
	dec.DisallowUnknownFields()
	var spec TableSpec
	if err := dec.Decode(&spec); err != nil {
		return nil, fmt.Errorf("JSON decode error: %w", err)
	}
	for i := range spec.Rows {
		if err := normalizeFilingTypes(&spec.Rows[i]); err != nil {
			return nil, fmt.Errorf("rows[%d]: %w", i, err)
		}
	}
	return &spec, nil
}

// walkNodes calls fn on every node in the YAML node tree, depth-first.
func walkNodes(n *yaml.Node, fn func(*yaml.Node) error) error {
	if n == nil {
		return nil
	}
	if err := fn(n); err != nil {
		return err
	}
	for _, child := range n.Content {
		if err := walkNodes(child, fn); err != nil {
			return err
		}
	}
	return nil
}

// normalizeFilingTypes converts the raw filing-type YAML value (string or list)
// into the FilingTypes string slice.
func normalizeFilingTypes(row *SpecRow) error {
	if row.RawFilingType == nil {
		return nil
	}
	switch v := row.RawFilingType.(type) {
	case string:
		row.FilingTypes = []string{v}
	case []interface{}:
		for _, item := range v {
			s, ok := item.(string)
			if !ok {
				return fmt.Errorf("filing-type list items must be strings")
			}
			row.FilingTypes = append(row.FilingTypes, s)
		}
	default:
		return fmt.Errorf("filing-type must be a string or list of strings")
	}
	return nil
}
