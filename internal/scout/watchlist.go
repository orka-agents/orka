/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package scout

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

var tableSeparatorCell = regexp.MustCompile(`^:?-+:?$`)

// WatchlistSourceIDs returns the source ids of a people-owned Markdown
// watchlist, in order, for Limits.SourceIDs. A table starts at a header row
// followed by a "| --- |" delimiter row and runs to the next blank line; every
// row in it, with or without outer pipes, must start with a source id cell. A
// pipe row outside a table, a row without a valid id, a repeated id, or a
// watchlist with no sources is an error, so a malformed watchlist cannot
// quietly shrink the set of sources a digest must report.
func WatchlistSourceIDs(markdown string) ([]string, error) {
	lines := strings.Split(strings.ReplaceAll(markdown, "\r\n", "\n"), "\n")
	var ids []string
	seen := map[string]struct{}{}
	inTable := false
	for index := 0; index < len(lines); index++ {
		line := strings.TrimSpace(lines[index])
		if line == "" {
			inTable = false
			continue
		}
		if !inTable {
			if index+1 < len(lines) && strings.Contains(line, "|") && isDelimiterRow(lines[index+1]) {
				inTable = true
				index++ // skip the delimiter row
				continue
			}
			if strings.HasPrefix(line, "|") {
				return nil, fmt.Errorf("watchlist line %d: table row outside a table with a header and delimiter row", index+1)
			}
			continue
		}
		id := tableCells(line)[0]
		if err := checkSourceID(id); err != nil {
			return nil, fmt.Errorf("watchlist line %d: source id %q %w", index+1, id, err)
		}
		if _, duplicate := seen[id]; duplicate {
			return nil, fmt.Errorf("watchlist line %d: source id %q is listed twice", index+1, id)
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		return nil, errors.New("watchlist lists no sources")
	}
	if len(ids) > MaxSources {
		return nil, fmt.Errorf("watchlist lists %d sources, maximum is %d", len(ids), MaxSources)
	}
	return ids, nil
}

// tableCells splits a table row, with or without outer pipes, into trimmed
// cells.
func tableCells(line string) []string {
	line = strings.TrimSpace(line)
	line = strings.TrimPrefix(line, "|")
	line = strings.TrimSuffix(line, "|")
	cells := strings.Split(line, "|")
	for index := range cells {
		cells[index] = strings.TrimSpace(cells[index])
	}
	return cells
}

func isDelimiterRow(line string) bool {
	line = strings.TrimSpace(line)
	if !strings.Contains(line, "|") {
		return false
	}
	for _, cell := range tableCells(line) {
		if !tableSeparatorCell.MatchString(cell) {
			return false
		}
	}
	return true
}
