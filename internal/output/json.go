package output

import (
	"encoding/json"
	"fmt"
	"io"
)

// JSONEnvelope is the structured output shape for --format json.
type JSONEnvelope struct {
	MarkdownTable   string                 `json:"markdown_table,omitempty"`
	Cells           []Cell                 `json:"cells,omitempty"`
	Citations       []Citation             `json:"citations,omitempty"`
	TaskID          string                 `json:"task_id,omitempty"`
	WatchURL        string                 `json:"watch_url,omitempty"`
	AppliedDefaults map[string]interface{} `json:"applied_defaults"`
}

// WriteJSON writes the JSON envelope to w.
func WriteJSON(w io.Writer, result *TableResult) error {
	applied := result.AppliedDefaults
	if applied == nil {
		applied = map[string]interface{}{}
	}
	env := JSONEnvelope{
		MarkdownTable:   result.MarkdownTable,
		Cells:           result.Cells,
		Citations:       result.Citations,
		TaskID:          result.TaskID,
		WatchURL:        result.WatchURL,
		AppliedDefaults: applied,
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(env); err != nil {
		return fmt.Errorf("encode JSON output: %w", err)
	}
	return nil
}
