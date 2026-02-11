/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package output

import (
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
)

// Table formats data as an aligned table with headers.
type Table struct {
	headers []string
	rows    [][]string
}

// NewTable creates a new table with the given column headers.
func NewTable(headers ...string) *Table {
	return &Table{
		headers: headers,
	}
}

// AddRow adds a row of values. Values count must match headers count.
func (t *Table) AddRow(values ...string) {
	t.rows = append(t.rows, values)
}

// Render writes the table to the given writer.
// Uses tabwriter with min width 0, tab width 8, padding 2, padchar space.
func (t *Table) Render(w io.Writer) error {
	tw := tabwriter.NewWriter(w, 0, 8, 2, ' ', 0)

	// Write headers in uppercase
	upper := make([]string, len(t.headers))
	for i, h := range t.headers {
		upper[i] = strings.ToUpper(h)
	}
	if _, err := fmt.Fprintln(tw, strings.Join(upper, "\t")); err != nil {
		return err
	}

	// Write rows
	for _, row := range t.rows {
		if _, err := fmt.Fprintln(tw, strings.Join(row, "\t")); err != nil {
			return err
		}
	}

	return tw.Flush()
}
