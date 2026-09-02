package service

import (
	"strconv"
	"strings"
	"testing"
)

func TestParseIntEnvUsesPlatformIntBounds(t *testing.T) {
	const name = "ORKA_TEST_PLATFORM_INT"

	t.Setenv(name, "")
	if got, err := parseIntEnv(name, 17); err != nil || got != 17 {
		t.Fatalf("parseIntEnv fallback = %d, %v", got, err)
	}

	t.Setenv(name, strconv.Itoa(int(^uint(0)>>1)))
	if _, err := parseIntEnv(name, 0); err != nil {
		t.Fatalf("parseIntEnv MaxInt: %v", err)
	}

	t.Setenv(name, "9223372036854775808")
	if _, err := parseIntEnv(name, 0); err == nil || !strings.Contains(err.Error(), "platform-sized integer") {
		t.Fatalf("parseIntEnv overflow error = %v", err)
	}

	t.Setenv(name, "+1")
	if _, err := parseIntEnv(name, 0); err == nil {
		t.Fatal("parseIntEnv accepted a non-canonical integer")
	}
}
