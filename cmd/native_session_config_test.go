package main

import (
	"errors"
	"flag"
	"strconv"
	"strings"
	"testing"

	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
)

func TestNativeSessionMaxBytesFromEnv(t *testing.T) {
	for _, test := range []struct {
		name    string
		raw     string
		want    int
		invalid bool
	}{
		{name: "default", want: harnessv2.DefaultMaxNativeSessionBytes},
		{name: "zero defaults", raw: "0", want: harnessv2.DefaultMaxNativeSessionBytes},
		{name: "custom", raw: " 1048576 ", want: 1 << 20},
		{name: "maximum", raw: strconv.Itoa(harnessv2.MaxNativeSessionBytes), want: harnessv2.MaxNativeSessionBytes},
		{name: "negative", raw: "-1", invalid: true},
		{name: "oversize", raw: strconv.Itoa(harnessv2.MaxNativeSessionBytes + 1), invalid: true},
		{name: "noninteger", raw: "8MiB", invalid: true},
		{name: "overflow", raw: strings.Repeat("9", 30), invalid: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("ORKA_NATIVE_SESSION_MAX_BYTES", test.raw)
			got, err := nativeSessionMaxBytesFromEnv()
			if test.invalid {
				if err == nil || !strings.Contains(err.Error(), "ORKA_NATIVE_SESSION_MAX_BYTES") {
					t.Fatalf("invalid environment error = %v", err)
				}
				return
			}
			if err != nil || got != test.want {
				t.Fatalf("native size = %d, %v; want %d", got, err, test.want)
			}
		})
	}
}

func TestNativeSessionSizeFlagOverridesInvalidEnvironment(t *testing.T) {
	for _, test := range []struct {
		name, argument string
		env            string
		want           int
		invalid        bool
	}{
		{name: "invalid environment omitted", env: "8MiB", invalid: true},
		{name: "explicit replaces malformed environment", env: "8MiB", argument: "16777216", want: 16 << 20},
		{name: "explicit replaces out of range environment", env: "67108865", argument: "8388608", want: 8 << 20},
		{name: "invalid explicit overrides valid environment", env: "8388608", argument: "-1", invalid: true},
		{name: "omitted uses valid environment", env: "16777216", want: 16 << 20},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("ORKA_NATIVE_SESSION_MAX_BYTES", test.env)
			value, envErr := nativeSessionMaxBytesFromEnv()
			flags := flag.NewFlagSet("native-size-test", flag.ContinueOnError)
			flags.IntVar(&value, "native-session-max-bytes", value, "")
			var args []string
			if test.argument != "" {
				args = []string{"--native-session-max-bytes=" + test.argument}
			}
			if err := flags.Parse(args); err != nil {
				t.Fatal(err)
			}
			got, err := effectiveNativeSessionMaxBytes(flags, value, envErr)
			if test.invalid {
				if err == nil {
					t.Fatal("invalid effective policy accepted")
				}
				if test.argument == "" && !errors.Is(err, envErr) {
					t.Fatal("environment error not retained")
				}
				return
			}
			if err != nil || got != test.want {
				t.Fatalf("effective policy=%d error=%v, want %d", got, err, test.want)
			}
		})
	}
}
