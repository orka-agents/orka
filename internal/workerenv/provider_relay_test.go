/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package workerenv

import "testing"

func TestProviderRelayRoundTripsAndRejectsWhatItCannotTrust(t *testing.T) {
	slots := map[string]ProviderRelayTarget{
		ProviderRelayPrimary:     {Namespace: "default", Name: "azure", UID: "uid-1"},
		ProviderRelayFallback(2): {Namespace: "default", Name: "azure-2", UID: "uid-2"},
	}
	encoded, err := EncodeProviderRelay(slots)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseProviderRelay(encoded)
	if err != nil || len(parsed) != 2 || parsed["fallback-2"] != slots["fallback-2"] || parsed["primary"] != slots["primary"] {
		t.Fatalf("ParseProviderRelay(%s) = %+v, %v", encoded, parsed, err)
	}
	if empty, err := EncodeProviderRelay(nil); err != nil || empty != "" {
		t.Fatalf("EncodeProviderRelay(nil) = %q, %v", empty, err)
	}
	if parsed, err := ParseProviderRelay(" "); err != nil || len(parsed) != 0 {
		t.Fatalf("ParseProviderRelay(empty) = %+v, %v", parsed, err)
	}
	for _, bad := range []string{
		`not json`,
		`{"primary":{"namespace":"default","name":"azure","uid":"u"}} {}`,
		`{"primary":{"namespace":"default","name":"azure","uid":"u"}} }`,
		`{"primary":{"namespace":"default","name":"azure","uid":"u"}} ]`,
		`{"secondary":{"namespace":"default","name":"azure","uid":"u"}}`,
		`{"fallback-01":{"namespace":"default","name":"azure","uid":"u"}}`,
		`{"fallback--1":{"namespace":"default","name":"azure","uid":"u"}}`,
		`{"primary":{"namespace":"default","name":"azure"}}`,
		`{"primary":{"namespace":"default","name":"azure","uid":"u","baseURL":"https://elsewhere"}}`,
	} {
		if _, err := ParseProviderRelay(bad); err == nil {
			t.Errorf("ParseProviderRelay(%s) accepted", bad)
		}
	}
	if _, err := EncodeProviderRelay(map[string]ProviderRelayTarget{"other": {Namespace: "a", Name: "b", UID: "c"}}); err == nil {
		t.Error("EncodeProviderRelay accepted an unknown slot")
	}
}

func TestProviderRelayURL(t *testing.T) {
	got := ProviderRelayURL("http://orka-controller.orka-system.svc:8080/", "team-a", "task-1", ProviderRelayFallback(0))
	want := "http://orka-controller.orka-system.svc:8080/internal/v1/tasks/team-a/task-1/providers/fallback-0"
	if got != want {
		t.Fatalf("ProviderRelayURL() = %q, want %q", got, want)
	}
}
