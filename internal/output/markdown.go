package output

import (
	"fmt"
	"io"
	"strings"
)

// WriteMarkdown writes the cited markdown table to w.
// In compact mode, the citations block is omitted.
func WriteMarkdown(w io.Writer, result *TableResult, compact bool) error {
	if result.MarkdownTable != "" {
		_, err := fmt.Fprintln(w, result.MarkdownTable)
		if err != nil {
			return err
		}
	} else {
		// Reconstruct markdown table from cells if the server didn't return it.
		if err := writeMarkdownFromCells(w, result); err != nil {
			return err
		}
	}

	if compact || len(result.Citations) == 0 {
		return nil
	}

	_, _ = fmt.Fprintln(w)
	_, _ = fmt.Fprintln(w, "**Citations**")
	for _, c := range result.Citations {
		line := fmt.Sprintf("[%s] %s", c.ID, c.Title)
		if c.FilingDate != "" {
			line += " (" + c.FilingDate
			if c.FilingType != "" {
				line += ", " + c.FilingType
			}
			line += ")"
		}
		if c.URL != "" {
			line += " — " + c.URL
		}
		if _, err := fmt.Fprintln(w, line); err != nil {
			return err
		}
	}
	return nil
}

// writeMarkdownFromCells constructs a minimal markdown table from cells when
// the server response does not include a pre-rendered markdown_table.
func writeMarkdownFromCells(w io.Writer, result *TableResult) error {
	if len(result.Cells) == 0 {
		return nil
	}
	// Gather unique row and col IDs in order.
	rowIDs := uniqueOrdered(result.Cells, func(c Cell) string { return c.RowID })
	colIDs := uniqueOrdered(result.Cells, func(c Cell) string { return c.ColID })

	// Index cells.
	index := map[string]string{}
	for _, c := range result.Cells {
		index[c.RowID+"|"+c.ColID] = c.Value
	}

	// Header row.
	header := "| Row | " + strings.Join(colIDs, " | ") + " |"
	sep := "|---|" + strings.Repeat("---|", len(colIDs))
	if _, err := fmt.Fprintln(w, header); err != nil {
		return err
	}
	if _, err := fmt.Fprintln(w, sep); err != nil {
		return err
	}
	for _, row := range rowIDs {
		parts := []string{row}
		for _, col := range colIDs {
			v := index[row+"|"+col]
			parts = append(parts, v)
		}
		if _, err := fmt.Fprintln(w, "| "+strings.Join(parts, " | ")+" |"); err != nil {
			return err
		}
	}
	return nil
}

func uniqueOrdered[T any](items []T, key func(T) string) []string {
	seen := map[string]bool{}
	var result []string
	for _, item := range items {
		k := key(item)
		if !seen[k] {
			seen[k] = true
			result = append(result, k)
		}
	}
	return result
}
