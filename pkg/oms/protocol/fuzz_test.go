package protocol

import (
	"bytes"
	"encoding/json"
	"fmt"
	"testing"
	"time"
)

func FuzzDecodeMutationEnvelope(f *testing.F) {
	request := MutationEnvelope{
		ProtocolVersion: Version, OperationID: "mop-fuzz-1", Binding: testBinding(), MemoryID: "mem-fuzz-1",
		Kind: MutationKindCreate, Generation: 1,
		State: &MutationState{Content: "fuzz", Tags: []string{"tag"}, Metadata: map[string]string{"owner": "orka"}},
	}
	if err := PrepareMutation(&request); err != nil {
		f.Fatal(err)
	}
	seed, _ := json.Marshal(request)
	f.Add(seed)
	f.Add([]byte(`{"protocolVersion":"orka.oms.v0alpha1"}`))
	f.Add([]byte{0xff, 0xfe})
	f.Fuzz(func(t *testing.T, body []byte) {
		decoded, err := DecodeMutationEnvelope(body)
		if err != nil {
			return
		}
		if err := ValidateMutationEnvelope(decoded); err != nil {
			t.Fatalf("accepted mutation failed validation: %v", err)
		}
		roundTrip, err := json.Marshal(decoded)
		if err != nil {
			t.Fatalf("marshal accepted mutation: %v", err)
		}
		if _, err := DecodeMutationEnvelope(roundTrip); err != nil {
			t.Fatalf("round-trip decode: %v", err)
		}
	})
}

func FuzzStrictJSONNeverAcceptsTrailingValue(f *testing.F) {
	request := CapabilitiesRequest{ProtocolVersion: Version, Binding: testBinding()}
	seed, _ := json.Marshal(request)
	f.Add(seed, []byte(`{}`))
	f.Add(seed, []byte(`null`))
	f.Fuzz(func(t *testing.T, first, trailing []byte) {
		if len(first)+len(trailing)+1 > MaxHTTPBodyBytes || len(bytes.TrimSpace(trailing)) == 0 {
			return
		}
		body := append(append(append([]byte(nil), first...), ' '), trailing...)
		if _, err := DecodeCapabilitiesRequest(body); err == nil {
			t.Fatalf("accepted trailing JSON: %q", body)
		}
	})
}

func FuzzValidateJSONUnicodeEscapesAfterEscapedDelimiter(f *testing.F) {
	f.Add(byte('"'), uint16(0xd83d), uint16(0xde00), true)
	f.Add(byte('\\'), uint16(0xd800), uint16(0), false)
	f.Add(byte('/'), uint16('A'), uint16(0xdc00), true)
	f.Fuzz(func(t *testing.T, escapeSelector byte, first, second uint16, includeSecond bool) {
		escapes := []byte{'"', '\\', '/', 'b', 'f', 'n', 'r', 't'}
		escape := escapes[int(escapeSelector)%len(escapes)]
		body := fmt.Appendf(nil, `{"value":"\%c\u%04x`, escape, first)
		if includeSecond {
			body = fmt.Appendf(body, `\u%04x`, second)
		}
		body = append(body, '"', '}')

		isHigh := func(value uint16) bool { return value >= 0xd800 && value <= 0xdbff }
		isLow := func(value uint16) bool { return value >= 0xdc00 && value <= 0xdfff }
		wantValid := true
		switch {
		case isHigh(first):
			wantValid = includeSecond && isLow(second)
		case isLow(first):
			wantValid = false
		case includeSecond && (isHigh(second) || isLow(second)):
			wantValid = false
		}

		err := validateJSONUnicodeEscapes(body)
		if wantValid && err != nil {
			t.Fatalf("valid escape sequence rejected: body=%s err=%v", body, err)
		}
		if !wantValid && err == nil {
			t.Fatalf("invalid surrogate sequence accepted: body=%s", body)
		}
	})
}

func FuzzValidateJSONNullabilityRejectsTypedNulls(f *testing.F) {
	f.Add(byte(0), "value")
	f.Add(byte(1), "map-value")
	f.Add(byte(2), "array-value")
	f.Fuzz(func(t *testing.T, selector byte, value string) {
		type fixture struct {
			Value  string            `json:"value"`
			Values map[string]string `json:"values"`
			Items  []string          `json:"items"`
		}
		wire := map[string]any{
			"value": value, "values": map[string]any{"key": value}, "items": []any{value},
		}
		switch selector % 3 {
		case 0:
			wire["value"] = nil
		case 1:
			wire["values"].(map[string]any)["key"] = nil
		case 2:
			wire["items"] = []any{nil}
		}
		body, err := json.Marshal(wire)
		if err != nil {
			t.Fatal(err)
		}
		var target fixture
		if err := ValidateJSONNullability(body, &target); err == nil {
			t.Fatalf("accepted typed null: %s", body)
		}
	})
}

func FuzzValidateSearchResponseSnapshotExpiry(f *testing.F) {
	f.Add(int64(-1), false)
	f.Add(int64(0), false)
	f.Add(int64(1), false)
	f.Add(int64(-time.Minute), true)
	f.Fuzz(func(t *testing.T, offsetNanos int64, exhausted bool) {
		now := time.Date(2026, time.August, 1, 12, 0, 0, 0, time.UTC)
		response := validSearchResponseForTest(now, exhausted)
		response.SnapshotExpiresAt = now.Add(time.Duration(offsetNanos))

		err := validateSearchResponseAt(&response, now)
		wantErr := !exhausted && offsetNanos <= 0
		if (err != nil) != wantErr {
			t.Fatalf("validateSearchResponseAt() error = %v, offset=%s exhausted=%v wantErr=%v",
				err, time.Duration(offsetNanos), exhausted, wantErr)
		}
		if err != nil || exhausted {
			return
		}

		localExpiry := now.Add(5 * time.Minute)
		continuationExpiry, ok := response.ContinuationExpiresAt(localExpiry)
		if !ok {
			t.Fatal("ContinuationExpiresAt() hid a validated nonterminal continuation")
		}
		if continuationExpiry.After(localExpiry) || continuationExpiry.After(response.SnapshotExpiresAt) {
			t.Fatalf("ContinuationExpiresAt() exceeded a bound: got=%v local=%v snapshot=%v",
				continuationExpiry, localExpiry, response.SnapshotExpiresAt)
		}
	})
}
