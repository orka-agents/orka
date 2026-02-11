/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package output

import (
	"encoding/json"
	"io"
)

// JSON writes the value as pretty-printed JSON to the writer.
func JSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// JSONCompact writes the value as compact JSON to the writer.
func JSONCompact(w io.Writer, v any) error {
	return json.NewEncoder(w).Encode(v)
}
