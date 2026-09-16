// Package envutil parses process environment variables with the canonical,
// fail-closed rules shared by the Orka binaries. Blank values always yield the
// fallback; malformed values are reported to the caller, which decides whether
// to exit or degrade.
package envutil

import (
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"time"
)

// String returns the trimmed value of name, or fallback when it is unset or blank.
func String(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}

// Int parses name as a canonical base-10 platform-sized integer. Non-canonical
// spellings such as a leading "+" or zero padding are rejected.
func Int(name string, fallback int) (int, error) {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback, nil
	}
	value, err := strconv.Atoi(raw)
	if err != nil || strconv.Itoa(value) != raw {
		return 0, fmt.Errorf("%s must be a canonical platform-sized integer", name)
	}
	return value, nil
}

// Int64 parses name as a canonical base-10 64-bit integer.
func Int64(name string, fallback int64) (int64, error) {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback, nil
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || strconv.FormatInt(value, 10) != raw {
		return 0, fmt.Errorf("%s must be a canonical integer", name)
	}
	return value, nil
}

// Duration parses name with time.ParseDuration.
func Duration(name string, fallback time.Duration) (time.Duration, error) {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback, nil
	}
	value, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("%s must be a duration", name)
	}
	return value, nil
}

// Bool parses name with strconv.ParseBool; blank is false.
func Bool(name string) (bool, error) {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return false, nil
	}
	value, err := strconv.ParseBool(raw)
	if err != nil {
		return false, fmt.Errorf("%s must be a boolean", name)
	}
	return value, nil
}

// MustPositiveInt is Int for process startup: a malformed or non-positive
// value exits the process with "invalid NAME".
func MustPositiveInt(name string, fallback int) int {
	value, err := Int(name, fallback)
	if err != nil || value < 1 {
		log.Fatalf("invalid %s", name)
	}
	return value
}

// MustInt64 is Int64 for process startup: a malformed value exits the
// process with "invalid NAME".
func MustInt64(name string, fallback int64) int64 {
	value, err := Int64(name, fallback)
	if err != nil {
		log.Fatalf("invalid %s", name)
	}
	return value
}

// MustDuration is Duration for process startup: a malformed value exits the
// process with "invalid NAME".
func MustDuration(name string, fallback time.Duration) time.Duration {
	value, err := Duration(name, fallback)
	if err != nil {
		log.Fatalf("invalid %s", name)
	}
	return value
}
