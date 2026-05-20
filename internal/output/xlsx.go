package output

import (
	"fmt"
	"io"
	"strings"

	"github.com/xuri/excelize/v2"
)

// WriteXLSX writes the table result as a flat xlsx grid to w.
// Sheet layout: Row 1 = header (row label + column names + Citations).
// No cell styling in Phase 1.
func WriteXLSX(w io.Writer, result *TableResult) error {
	f := excelize.NewFile()
	defer func() { _ = f.Close() }()

	sheet := "Sheet1"

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

	// Header row.
	headers := append([]string{"Row"}, colIDs...)
	headers = append(headers, "Citations")
	for col, h := range headers {
		cell, err := excelize.CoordinatesToCellName(col+1, 1)
		if err != nil {
			return fmt.Errorf("xlsx header coordinate: %w", err)
		}
		if err := f.SetCellValue(sheet, cell, h); err != nil {
			return fmt.Errorf("xlsx set header: %w", err)
		}
	}

	// Data rows.
	for rowIdx, row := range rowIDs {
		excelRow := rowIdx + 2

		// Row label.
		rowCell, _ := excelize.CoordinatesToCellName(1, excelRow)
		_ = f.SetCellValue(sheet, rowCell, row)

		allCitations := map[string]bool{}
		for colIdx, col := range colIDs {
			entry := index[row+"|"+col]
			c, _ := excelize.CoordinatesToCellName(colIdx+2, excelRow)
			_ = f.SetCellValue(sheet, c, entry.value)
			for _, cid := range entry.citations {
				allCitations[cid] = true
			}
		}

		// Citations column (last).
		var cids []string
		for cid := range allCitations {
			cids = append(cids, cid)
		}
		citCell, _ := excelize.CoordinatesToCellName(len(colIDs)+2, excelRow)
		_ = f.SetCellValue(sheet, citCell, strings.Join(cids, ", "))
	}

	if _, err := f.WriteTo(w); err != nil {
		return fmt.Errorf("xlsx write: %w", err)
	}
	return nil
}
