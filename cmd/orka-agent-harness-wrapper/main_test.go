package main

import (
	"strings"
	"testing"
)

func TestRunRejectsRawBearerTokenFlagWithoutEchoingValue(t *testing.T) {
	const token = "raw-wrapper-bearer-must-not-reach-argv"
	err := run([]string{"--bearer-token=" + token})
	if err == nil || !strings.Contains(err.Error(), "flag provided but not defined") {
		t.Fatalf("run() error = %v, want unknown bearer-token flag", err)
	}
	if strings.Contains(err.Error(), token) {
		t.Fatal("run() error exposed rejected bearer token")
	}
}
