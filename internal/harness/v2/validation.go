package v2

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	MaxProtocolStringBytes  = 4 * 1024
	MaxDiagnosticBytes      = 8 * 1024
	MaxMetadataEntries      = 128
	MaxContentBlocks        = 128
	MaxPromptContentBytes   = 1 << 20
	MaxResourceURIBytes     = 4 * 1024
	MaxContentNameBytes     = 1024
	MaxContentMIMETypeBytes = 256
	MaxRawConfigBytes       = 256 << 10
)

func validateProtocol(version string) error {
	if version != ProtocolVersion {
		return fmt.Errorf("protocol version %q is unsupported; want %q", version, ProtocolVersion)
	}
	return nil
}

func validateBoundedString(name, value string, required bool, maxBytes int) error {
	if !utf8.ValidString(value) {
		return fmt.Errorf("%s contains invalid UTF-8", name)
	}
	if required && strings.TrimSpace(value) == "" {
		return fmt.Errorf("%s is required", name)
	}
	if len(value) > maxBytes {
		return fmt.Errorf("%s exceeds %d bytes", name, maxBytes)
	}
	return nil
}

func validateTimestamp(name string, value time.Time) error {
	if value.IsZero() {
		return fmt.Errorf("%s is required", name)
	}
	if value.Location() == nil {
		return fmt.Errorf("%s has no location", name)
	}
	return nil
}

func ensureJSONEOF(dec *json.Decoder) error {
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		if err == nil {
			return fmt.Errorf("contains trailing JSON value")
		}
		return fmt.Errorf("contains trailing data: %w", err)
	}
	return nil
}

func exactlyOne(values ...bool) bool {
	count := 0
	for _, value := range values {
		if value {
			count++
		}
	}
	return count == 1
}
