package output

import (
	"encoding/csv"
	"io"
	"strings"
)

// WriteCSV writes the table result as CSV to w.
// Header row: row_id, then column IDs, then "Citations".
func WriteCSV(w io.Writer, result *TableResult) error {
	cw := csv.NewWriter(w)
	defer cw.Flush()

	rowIDs := uniqueOrdered(result.Cells, func(c Cell) string { return c.RowID })
	colIDs := uniqueOrdered(result.Cells, func(c Cell) string { return c.ColID })

	// Build cell index.
	type cellEntry struct {
		value     string
		citations []string
	}
	index := map[string]cellEntry{}
	for _, c := range result.Cells {
		index[c.RowID+"|"+c.ColID] = cellEntry{value: c.Value, citations: c.Citations}
	}

	// Header.
	header := append([]string{"row"}, colIDs...)
	header = append(header, "Citations")
	if err := cw.Write(header); err != nil {
		return err
	}

	// Rows.
	for _, row := range rowIDs {
		record := []string{row}
		allCitations := map[string]bool{}
		for _, col := range colIDs {
			entry := index[row+"|"+col]
			record = append(record, entry.value)
			for _, cid := range entry.citations {
				allCitations[cid] = true
			}
		}
		// Collect citation IDs for this row.
		var cids []string
		for cid := range allCitations {
			cids = append(cids, cid)
		}
		record = append(record, strings.Join(cids, ","))
		if err := cw.Write(record); err != nil {
			return err
		}
	}
	return nil
}
