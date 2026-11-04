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
// watchlist, in order, for Limits.SourceIDs. Every Markdown table row other
// than a header row (the row directly above a "| --- |" separator) must start
// with a source id cell. A row that does not, a repeated id, or a watchlist
// with no sources is an error, so a malformed watchlist cannot quietly shrink
// the set of sources a digest must report.
func WatchlistSourceIDs(markdown string) ([]string, error) {
	lines := strings.Split(strings.ReplaceAll(markdown, "\r\n", "\n"), "\n")
	var ids []string
	seen := map[string]struct{}{}
	for index, line := range lines {
		cells, ok := tableCells(line)
		if !ok || isSeparatorRow(cells) {
			continue
		}
		if index+1 < len(lines) {
			if next, ok := tableCells(lines[index+1]); ok && isSeparatorRow(next) {
				continue
			}
		}
		id := cells[0]
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

func tableCells(line string) ([]string, bool) {
	line = strings.TrimSpace(line)
	if !strings.HasPrefix(line, "|") {
		return nil, false
	}
	cells := strings.Split(strings.Trim(line, "|"), "|")
	for index := range cells {
		cells[index] = strings.TrimSpace(cells[index])
	}
	return cells, true
}

func isSeparatorRow(cells []string) bool {
	for _, cell := range cells {
		if !tableSeparatorCell.MatchString(cell) {
			return false
		}
	}
	return true
}
