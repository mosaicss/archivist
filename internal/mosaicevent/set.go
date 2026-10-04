package mosaicevent

import (
	"encoding/json"
	"fmt"
	"sort"
)

// Set holds one parser per supported version (each from its own vendored bundle) and validates
// an input with the version it declares, as the relay does: mosaic-event/1 inputs go to the v1
// bundle, mosaic-event/2 inputs to the v2 bundle, mosaic-event/3 inputs to the v3 bundle,
// anything else is refused.
type Set struct {
	parsers map[string]*Parser
}

// NewSet loads and verifies every embedded bundle.
func NewSet() (*Set, error) {
	s := &Set{parsers: map[string]*Parser{}}
	for version := range bundles {
		p, err := NewVersion(version)
		if err != nil {
			return nil, fmt.Errorf("load %s contract: %w", version, err)
		}
		s.parsers[version] = p
	}
	return s, nil
}

// Parser returns the parser for version, or nil when the version is unsupported.
func (s *Set) Parser(version string) *Parser { return s.parsers[version] }

// Versions lists the supported versions, sorted.
func (s *Set) Versions() []string {
	out := make([]string, 0, len(s.parsers))
	for v := range s.parsers {
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

// Parse validates raw with the parser its schemaVersion names: the envelope's schemaVersion, or
// for a chunk the start chunk's messageMetadata.schemaVersion. Other chunks carry no version;
// validate those with Parser(version).
func (s *Set) Parse(raw []byte, kind string) (*Parsed, error) {
	version, err := declaredVersion(raw, kind)
	if err != nil {
		return nil, err
	}
	p := s.parsers[version]
	if p == nil {
		return nil, fmt.Errorf("unsupported schemaVersion %q", version)
	}
	return p.Parse(raw, kind)
}

func declaredVersion(raw []byte, kind string) (string, error) {
	switch kind {
	case "envelope":
		var env struct {
			SchemaVersion string `json:"schemaVersion"`
		}
		if err := json.Unmarshal(raw, &env); err != nil {
			return "", err
		}
		return env.SchemaVersion, nil
	case "chunk":
		var chunk struct {
			Type     string `json:"type"`
			Metadata struct {
				SchemaVersion string `json:"schemaVersion"`
			} `json:"messageMetadata"`
		}
		if err := json.Unmarshal(raw, &chunk); err != nil {
			return "", err
		}
		if chunk.Type != "start" {
			return "", fmt.Errorf("a %q chunk carries no schemaVersion; validate it with Parser(version)", chunk.Type)
		}
		return chunk.Metadata.SchemaVersion, nil
	}
	return "", fmt.Errorf("unknown contract kind %q", kind)
}
