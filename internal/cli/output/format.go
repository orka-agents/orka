/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package output

import (
	"fmt"
	"io"
	"time"
)

// Format represents an output format.
type Format string

const (
	FormatTable Format = "table"
	FormatJSON  Format = "json"
	FormatYAML  Format = "yaml"
)

// ParseFormat parses a string into a Format, returning an error for unknown formats.
func ParseFormat(s string) (Format, error) {
	switch s {
	case "table":
		return FormatTable, nil
	case "json":
		return FormatJSON, nil
	case "yaml":
		return FormatYAML, nil
	default:
		return "", fmt.Errorf("unknown output format: %q (supported: table, json, yaml)", s)
	}
}

// FormatAge returns a human-readable age string from a timestamp.
// e.g., "5m", "2h", "3d"
func FormatAge(t time.Time) string {
	return FormatDuration(time.Since(t))
}

// FormatDuration returns a human-readable duration string.
// e.g., "5s", "2m30s", "1h5m"
func FormatDuration(d time.Duration) string {
	if d < 0 {
		d = -d
	}

	days := int(d.Hours() / 24)
	if days > 0 {
		return fmt.Sprintf("%dd", days)
	}

	hours := int(d.Hours())
	if hours > 0 {
		mins := int(d.Minutes()) % 60
		if mins > 0 {
			return fmt.Sprintf("%dh%dm", hours, mins)
		}
		return fmt.Sprintf("%dh", hours)
	}

	mins := int(d.Minutes())
	if mins > 0 {
		secs := int(d.Seconds()) % 60
		if secs > 0 {
			return fmt.Sprintf("%dm%ds", mins, secs)
		}
		return fmt.Sprintf("%dm", mins)
	}

	secs := int(d.Seconds())
	if secs > 0 {
		return fmt.Sprintf("%ds", secs)
	}

	return "0s"
}

// PrintResult prints data in the specified format.
// For "table" format, the caller should use the Table type directly.
// For "json", uses JSON pretty printer.
// For "yaml", returns an error (not yet supported).
func PrintResult(w io.Writer, format Format, data any) error {
	switch format {
	case FormatTable:
		return fmt.Errorf("table format requires using the Table type directly")
	case FormatJSON:
		return JSON(w, data)
	case FormatYAML:
		return fmt.Errorf("yaml output format is not yet supported")
	default:
		return fmt.Errorf("unknown output format: %q", format)
	}
}
