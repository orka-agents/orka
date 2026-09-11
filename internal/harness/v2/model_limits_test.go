/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package v2

import (
	"bytes"
	"strings"
	"testing"
)

func TestRuntimeProfileValidatesOptionalModelLimits(t *testing.T) {
	profile := testRuntimeProfile()
	profile.ProviderKind = "opencode"
	profile.Model = "openai/test-model"
	profile.AdapterDigests = map[string]string{"opencode": testSHA256("adapter")}
	profile.ProxyCredentialScope = "model:openai/test-model"

	if err := profile.Validate(); err != nil {
		t.Fatalf("Validate() error = %v, want external profile compatibility without limits", err)
	}
	profile.ModelLimits = &ModelTokenLimits{Context: 4096, Output: 4096}
	if err := profile.Validate(); err == nil || !strings.Contains(err.Error(), "must exceed") {
		t.Fatalf("Validate() error = %v, want inverted limits rejection", err)
	}
	profile.ModelLimits = &ModelTokenLimits{Context: 32768, Output: 4096}
	if err := profile.Validate(); err != nil {
		t.Fatalf("Validate() error = %v, want valid OpenCode profile", err)
	}
}

func TestCanonicalProfileDigestIncludesOptionalModelLimits(t *testing.T) {
	profile := testRuntimeProfile()
	withoutLimits, err := CanonicalProfileDigest(profile)
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := CanonicalValue(profile)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(canonical, []byte("modelLimits")) {
		t.Fatalf("nil model limits changed existing profile canonical form: %s", canonical)
	}

	profile.ModelLimits = &ModelTokenLimits{Context: 32768, Output: 4096}
	withLimits, err := CanonicalProfileDigest(profile)
	if err != nil {
		t.Fatal(err)
	}
	if withLimits == withoutLimits {
		t.Fatal("profile digest did not change when model limits were added")
	}
	profile.ModelLimits.Context++
	changedContext, err := CanonicalProfileDigest(profile)
	if err != nil {
		t.Fatal(err)
	}
	if changedContext == withLimits {
		t.Fatal("profile digest did not change with model context limit")
	}
	profile.ModelLimits.Context--
	profile.ModelLimits.Output++
	changedOutput, err := CanonicalProfileDigest(profile)
	if err != nil {
		t.Fatal(err)
	}
	if changedOutput == withLimits {
		t.Fatal("profile digest did not change with model output limit")
	}
}
