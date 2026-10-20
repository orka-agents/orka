package v2

import (
	"bytes"
	"encoding/json"
	"testing"
)

func TestBrokeredApprovalCapabilityRemainsOptInOnTheWire(t *testing.T) {
	provider := ProviderCapabilities{ProviderKinds: []string{"codex"}, SupportsCancel: true, SupportsTools: true}
	body, err := json.Marshal(provider)
	if err != nil {
		t.Fatal(err)
	}
	// Older controllers reject unknown capability fields. An unqualified
	// supervisor must still decode against their approval-free wire contract.
	var legacy struct {
		ProviderKinds       []string `json:"providerKinds"`
		SupportsPermissions bool     `json:"supportsPermissions"`
		SupportsCancel      bool     `json:"supportsCancel"`
		SupportsTools       bool     `json:"supportsTools"`
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&legacy); err != nil {
		t.Fatalf("unqualified capability broke the older controller: %v", err)
	}
	provider.SupportsBrokeredToolApprovals = true
	body, err = json.Marshal(provider)
	if err != nil {
		t.Fatal(err)
	}
	var advertised map[string]any
	if err := json.Unmarshal(body, &advertised); err != nil || advertised["supportsBrokeredToolApprovals"] != true {
		t.Fatalf("qualified capability was not advertised: %s, %v", body, err)
	}
}
