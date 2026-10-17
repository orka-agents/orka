/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package protocol

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func TestSanitizeExtensionsRejectsUnsafeIdentifiersWithoutRewriting(t *testing.T) {
	for name, value := range map[string]string{
		"empty":                "",
		"blank":                " \t\n",
		"leading space":        " future.v1",
		"trailing space":       "future.v1\u00a0",
		"embedded NUL":         "future.\x00v1",
		"embedded newline":     "future.\nv1",
		"embedded tab":         "future.\tv1",
		"DEL":                  "future.\x7fv1",
		"C1 control":           "future.\u0085v1",
		"invalid UTF8":         string([]byte{'f', 0xff}),
		"too long":             strings.Repeat("a", MaxIdentityBytes+1),
		"too many UTF8 bytes":  strings.Repeat("é", MaxIdentityBytes/2+1),
		"placeholder":          "[REDACTED]",
		"embedded placeholder": "future.[REDACTED].v1",
		"authorization":        "Authorization: Bearer extension-fixture-value",
		"basic authorization":  "Authorization: Basic Zml4dHVyZTpwYXNz",
		"transaction token":    "Txn-Token: extension-fixture-value",
		"cookie":               "Cookie: session=extension-fixture-value",
		"secret assignment":    "api_key=extension-fixture-value",
		"quoted assignment":    `{"client_secret":"extension-fixture-value"}`,
		"natural language":     "token is extension-fixture-value",
		"OpenAI token":         "sk-abcdefghijklmnopqrstuvwxyz",
		"GitHub token":         "ghp_abcdefghijklmnopqrstuvwxyz",
		"GitHub PAT":           "github_pat_abcdefghijklmnopqrstuvwxyz123456",
		"Slack token":          "xoxb-abcdefghijklmnopqrstuvwxyz",
		"JWT":                  "eyJabcdefghijk.abcdefghijk.abcdefghijk",
		"URL userinfo":         "https://fixture-user@example.invalid/extension",
		"signed URL":           "https://example.invalid/extension?sig=fixture-signature",
	} {
		t.Run(name, func(t *testing.T) {
			if got := SanitizeExtensions([]string{value}); got != nil {
				t.Fatalf("unsafe extension retained or rewritten: %q", got)
			}
		})
	}
}

func TestSanitizeExtensionsDropsJSONUnicodeRepairs(t *testing.T) {
	for _, value := range []string{`"future.\ud800.v1"`, `"future.\udc00.v1"`, `"future.�.v1"`, "\"future.\xff.v1\""} {
		response, err := DecodeCapabilities([]byte(`{"protocolVersion":"orka.gateway.v1","adapterName":"test","capabilities":{"idempotentDelivery":true,"extensions":[` + value + `,"future.safe.v1"]}}`))
		if err != nil {
			t.Fatal(err)
		}
		if got := SanitizeExtensions(response.Capabilities.Extensions); !reflect.DeepEqual(got, []string{"future.safe.v1"}) {
			t.Fatalf("retained Unicode replacement identifier: %q", got)
		}
	}
}

func TestSanitizeExtensionsPreservesUnknownSafeIdentifiers(t *testing.T) {
	values := []string{
		"future.vendor.v42", "future/vendor+feature:v2", "未知の拡張.é", "Future Extension v1",
		"safe%2Fextension\\u0026", strings.Repeat("a", MaxIdentityBytes), strings.Repeat("é", MaxIdentityBytes/2),
	}
	got := SanitizeExtensions(values)
	if !reflect.DeepEqual(got, values) {
		t.Fatalf("safe unknown extensions changed: got %q, want %q", got, values)
	}
	got[0] = "changed-output"
	if values[0] != "future.vendor.v42" {
		t.Fatal("SanitizeExtensions aliased input slice")
	}
	values[1] = "changed-input"
	if got[1] != "future/vendor+feature:v2" {
		t.Fatal("SanitizeExtensions retained caller-owned storage")
	}
}

func TestSanitizeExtensionsDeduplicatesAndBoundsRetainedEntries(t *testing.T) {
	values := []string{"", " future.v1", "future.v1", "future.v1"}
	want := []string{"future.v1"}
	for index := 1; index < 20; index++ {
		value := fmt.Sprintf("future.v%d", index+1)
		values = append(values, value, value)
		if index < 16 {
			want = append(want, value)
		}
	}
	if got := SanitizeExtensions(values); !reflect.DeepEqual(got, want) {
		t.Fatalf("bounded extensions = %q, want %q", got, want)
	}
}

func TestSanitizeExtensionsOmitsEmptyLists(t *testing.T) {
	for _, values := range [][]string{nil, {}, {"", "[REDACTED]"}} {
		if got := SanitizeExtensions(values); got != nil {
			t.Fatalf("empty extensions = %q, want nil", got)
		}
	}
}
