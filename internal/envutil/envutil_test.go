package envutil

import (
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestIntUsesPlatformIntBounds(t *testing.T) {
	const name = "ORKA_TEST_PLATFORM_INT"

	t.Setenv(name, "")
	if got, err := Int(name, 17); err != nil || got != 17 {
		t.Fatalf("Int fallback = %d, %v", got, err)
	}

	t.Setenv(name, strconv.Itoa(int(^uint(0)>>1)))
	if _, err := Int(name, 0); err != nil {
		t.Fatalf("Int MaxInt: %v", err)
	}

	t.Setenv(name, "9223372036854775808")
	if _, err := Int(name, 0); err == nil || !strings.Contains(err.Error(), "platform-sized integer") {
		t.Fatalf("Int overflow error = %v", err)
	}

	t.Setenv(name, "+1")
	if _, err := Int(name, 0); err == nil {
		t.Fatal("Int accepted a non-canonical integer")
	}
}

func TestInt64RejectsNonCanonical(t *testing.T) {
	const name = "ORKA_TEST_INT64"
	for _, raw := range []string{"007", "+5", "1.0", "x"} {
		t.Setenv(name, raw)
		if _, err := Int64(name, 0); err == nil || !strings.Contains(err.Error(), "canonical integer") {
			t.Fatalf("Int64(%q) error = %v", raw, err)
		}
	}
	t.Setenv(name, " 42 ")
	if got, err := Int64(name, 0); err != nil || got != 42 {
		t.Fatalf("Int64 = %d, %v", got, err)
	}
}

func TestDurationAndBool(t *testing.T) {
	const name = "ORKA_TEST_DURATION"
	t.Setenv(name, "")
	if got, err := Duration(name, time.Minute); err != nil || got != time.Minute {
		t.Fatalf("Duration fallback = %s, %v", got, err)
	}
	t.Setenv(name, "1h30m")
	if got, err := Duration(name, 0); err != nil || got != 90*time.Minute {
		t.Fatalf("Duration = %s, %v", got, err)
	}
	t.Setenv(name, "soon")
	if _, err := Duration(name, 0); err == nil || !strings.Contains(err.Error(), "must be a duration") {
		t.Fatalf("Duration error = %v", err)
	}

	const flag = "ORKA_TEST_BOOL"
	t.Setenv(flag, "")
	if got, err := Bool(flag); err != nil || got {
		t.Fatalf("Bool blank = %v, %v", got, err)
	}
	t.Setenv(flag, "true")
	if got, err := Bool(flag); err != nil || !got {
		t.Fatalf("Bool true = %v, %v", got, err)
	}
	t.Setenv(flag, "yes")
	if _, err := Bool(flag); err == nil || !strings.Contains(err.Error(), "must be a boolean") {
		t.Fatalf("Bool error = %v", err)
	}
}

func TestStringTrimsAndFallsBack(t *testing.T) {
	const name = "ORKA_TEST_STRING"
	t.Setenv(name, "  ")
	if got := String(name, "fallback"); got != "fallback" {
		t.Fatalf("String blank = %q", got)
	}
	t.Setenv(name, " value ")
	if got := String(name, "fallback"); got != "value" {
		t.Fatalf("String = %q", got)
	}
}
