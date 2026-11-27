package envutil

import (
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"time"
)

func String(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}

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

func MustInt(name string, fallback int) int {
	value, err := Int(name, fallback)
	if err != nil {
		log.Fatalf("invalid %s", name)
	}
	return value
}

func MustPositiveInt(name string, fallback int) int {
	value, err := Int(name, fallback)
	if err != nil || value < 1 {
		log.Fatalf("invalid %s", name)
	}
	return value
}

func MustInt64(name string, fallback int64) int64 {
	value, err := Int64(name, fallback)
	if err != nil {
		log.Fatalf("invalid %s", name)
	}
	return value
}

func MustDuration(name string, fallback time.Duration) time.Duration {
	value, err := Duration(name, fallback)
	if err != nil {
		log.Fatalf("invalid %s", name)
	}
	return value
}
