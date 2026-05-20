package cascade

import (
	"encoding/json"
	"fmt"
)

// CascadeRulesFile mirrors the top-level structure of cascade-rules.json.
type CascadeRulesFile struct {
	Version           string              `json:"version"`
	Generated         string              `json:"generated"`
	Source            string              `json:"source"`
	DateSpanMaxDays   int                 `json:"date_span_max_days"`
	ExchangesByCountry map[string][]string `json:"exchanges_by_country"`
	SedarFilingTypes  []string            `json:"sedar_filing_types"`
	SecFilingTypes    []string            `json:"sec_filing_types"`
	CascadeRules      []CascadeRuleMeta   `json:"cascade_rules"`
	DimensionGraph    map[string]DimNode  `json:"dimension_graph"`
}

// CascadeRuleMeta is the per-rule metadata entry in cascade-rules.json.
type CascadeRuleMeta struct {
	Rule        string `json:"rule"`
	Description string `json:"description"`
	Severity    string `json:"severity"`
}

// DimNode is one entry in the dimension_graph.
type DimNode struct {
	Implies        []string `json:"implies"`
	Lock           bool     `json:"lock"`
	ProjectionOnly bool     `json:"projection_only"`
}

// rules is the parsed cascade-rules.json, populated by init().
var rules CascadeRulesFile

// allFilingTypes is the union of SEC + SEDAR types for O(1) validation.
var allFilingTypes map[string]struct{}

func init() {
	if err := json.Unmarshal(cascadeRulesJSON, &rules); err != nil {
		panic(fmt.Sprintf("cascade: malformed cascade-rules.json: %v", err))
	}
	if rules.Version == "" {
		panic("cascade: cascade-rules.json missing version field")
	}
	allFilingTypes = make(map[string]struct{}, len(rules.SedarFilingTypes)+len(rules.SecFilingTypes))
	for _, t := range rules.SedarFilingTypes {
		allFilingTypes[t] = struct{}{}
	}
	for _, t := range rules.SecFilingTypes {
		allFilingTypes[t] = struct{}{}
	}
}

// loadedRules returns the parsed rules for use by validate.go and tests.
func loadedRules() *CascadeRulesFile {
	return &rules
}
