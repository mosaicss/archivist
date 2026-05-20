// Package output renders archivist table results in markdown, JSON, CSV, and xlsx.
package output

// TableResult is the parsed table response from chat-api /table/search.
type TableResult struct {
	// MarkdownTable is the full markdown table string (may be empty; cells are canonical).
	MarkdownTable string `json:"markdown_table,omitempty"`
	// Cells contains the structured cell data.
	Cells []Cell `json:"cells,omitempty"`
	// Citations contains all citations referenced by cells.
	Citations []Citation `json:"citations,omitempty"`
	// TaskID is the persistent session identifier.
	TaskID string `json:"task_id,omitempty"`
	// WatchURL is the web audit URL for this session.
	WatchURL string `json:"watch_url,omitempty"`
	// AppliedDefaults contains defaults the server filled in.
	AppliedDefaults map[string]interface{} `json:"applied_defaults,omitempty"`
}

// Cell is one cross of a row and column in the table result.
type Cell struct {
	RowID       string     `json:"row_id"`
	ColID       string     `json:"col_id"`
	Value       string     `json:"value"`
	Citations   []string   `json:"citations,omitempty"`
	Confidence  float64    `json:"confidence,omitempty"`
	RefCitations []Citation `json:"-"` // resolved at render time
}

// Citation is a source reference used by cells.
type Citation struct {
	ID          string `json:"id"`
	Title       string `json:"title,omitempty"`
	URL         string `json:"url,omitempty"`
	FilingDate  string `json:"filing_date,omitempty"`
	FilingType  string `json:"filing_type,omitempty"`
	IssuerKey   string `json:"issuer_key,omitempty"`
}

// Format enumerates output format options.
type Format string

const (
	FormatMarkdown Format = "markdown"
	FormatJSON     Format = "json"
	FormatCSV      Format = "csv"
	FormatXLSX     Format = "xlsx"
)

// ParseFormat converts a --format flag value to a Format constant.
// Returns FormatMarkdown for unrecognized values.
func ParseFormat(s string) Format {
	switch s {
	case "json":
		return FormatJSON
	case "csv":
		return FormatCSV
	case "xlsx":
		return FormatXLSX
	default:
		return FormatMarkdown
	}
}
