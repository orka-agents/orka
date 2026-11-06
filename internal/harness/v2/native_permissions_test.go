package v2

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestNativePermissionModeIsPartOfTheSessionLaunchConfiguration(t *testing.T) {
	profile := testRuntimeProfile()
	preapproved := testMCPPolicyConfiguration()
	if err := preapproved.ValidateProfile(profile); err != nil {
		t.Fatalf("pre-approved configuration: %v", err)
	}
	encoded, err := json.Marshal(preapproved)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "nativePermissions") {
		t.Fatalf("pre-approved configuration changed its wire form: %s", encoded)
	}

	ask := preapproved
	ask.NativePermissions = NativePermissionsAsk
	if err := ask.ValidateProfile(profile); err != nil {
		t.Fatalf("ask configuration: %v", err)
	}
	if ask.Matches(preapproved) || preapproved.Matches(ask) {
		t.Fatal("configurations with different native permission modes match")
	}
	if !ask.Matches(ask) {
		t.Fatal("ask configuration does not match itself")
	}

	unknown := preapproved
	unknown.NativePermissions = "allow-all"
	if err := unknown.ValidateProfile(profile); err == nil || !strings.Contains(err.Error(), "native permission mode") {
		t.Fatalf("unknown native permission mode error = %v", err)
	}
}

func TestPromptMCPAuthorizationCarriesNativePermissionMode(t *testing.T) {
	metadata := testMutationMetadata(t, true)
	lease := PromptLease{Generation: 1, IssuedAt: testNow, ExpiresAt: testNow.Add(2 * time.Minute)}
	authorization := testPromptMCPAuthorization(metadata, lease)
	authorization.NativePermissions = NativePermissionsAsk
	session := testMCPPolicyConfiguration()
	if authorization.Configuration().Matches(session) {
		t.Fatal("an ask prompt matches a pre-approved session")
	}
	session.NativePermissions = NativePermissionsAsk
	if !authorization.Configuration().Matches(session) {
		t.Fatal("an ask prompt does not match its ask session")
	}
	if err := authorization.ValidateForAt(metadata, lease, testNow.Add(time.Second)); err != nil {
		t.Fatalf("ValidateForAt() error = %v", err)
	}
	authorization.NativePermissions = "allow-all"
	if err := authorization.ValidateForAt(metadata, lease, testNow.Add(time.Second)); err == nil {
		t.Fatal("prompt authorization accepted an unknown native permission mode")
	}
}
