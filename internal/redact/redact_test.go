/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package redact

import (
	"strings"
	"testing"
)

func TestSensitiveTextPreservesRedactedMarkdown(t *testing.T) {
	value := strings.Repeat("a", 32)
	for input, want := range map[string]string{
		"Environment assignment `API_KEY=\"" + value + "\"`.":    "Environment assignment `API_KEY=\"[REDACTED]\"`.",
		"Environment assignment `API_KEY='" + value + "'`.":      "Environment assignment `API_KEY='[REDACTED]'`.",
		`{"api_key":"` + value + `","line":12}`:                  `{"api_key":"[REDACTED]","line":12}`,
		`{"api_key":"a\"opaque-tail","line":12}`:                 `{"api_key":"[REDACTED]","line":12}`,
		`{"api_key":"a\\\"opaque-tail","line":12}`:               `{"api_key":"[REDACTED]","line":12}`,
		`API_KEY='a\'opaque-tail' followed by text`:              `API_KEY='[REDACTED]' followed by text`,
		`API_KEY='opaque-shell-placeholder\'`:                    `API_KEY='[REDACTED]'`,
		`API_KEY='opaque-shell-placeholder\' followed by text`:   `API_KEY='[REDACTED]' followed by text`,
		`API_KEY="opaque-literal-placeholder\"`:                  `API_KEY="[REDACTED]"`,
		`API_KEY="opaque-literal-placeholder\" followed by text`: `API_KEY="[REDACTED]" followed by text`,
	} {
		if got := SensitiveText(input); got != want || SensitiveText(got) != got {
			t.Fatal("redaction changed delimiters outside the credential or was not stable on a second pass")
		}
	}
	input := "API_KEY=" + redactedValue + strings.Repeat("a", 32)
	if got := SensitiveText(input); got != "API_KEY="+redactedValue {
		t.Fatal("a credential using the redaction marker as a prefix was not fully redacted")
	}
}

func TestSensitiveTextRedactsTxnTokenHeader(t *testing.T) {
	input := `curl -H "Txn-Token: opaque-secret-token" https://orka.example.test`
	got := SensitiveText(input)
	if strings.Contains(got, "opaque-secret-token") {
		t.Fatalf("SensitiveText leaked Txn-Token value: %q", got)
	}
	if !strings.Contains(got, "Txn-Token: "+redactedValue) {
		t.Fatalf("SensitiveText() = %q, want redacted Txn-Token header", got)
	}
}

func TestSensitiveTextRedactsJWT(t *testing.T) {
	input := "token eyJhbGciOiJSUzI1NiIsInR5cCI6InR4bnRva2VuK2p3dCJ9.eyJzdWIiOiJ3b3JrbG9hZCIsInR4biI6InR4bi0xMjMifQ.signaturevalue1234567890"
	got := SensitiveText(input)
	if strings.Contains(got, "eyJhbGci") {
		t.Fatalf("SensitiveText leaked JWT: %q", got)
	}
}

func TestSensitiveTextRedactsURLUserInfoWithoutPassword(t *testing.T) {
	input := `repo https://token@example.com/org/repo.git`
	got := SensitiveText(input)
	if strings.Contains(got, "token@example") {
		t.Fatalf("SensitiveText leaked URL userinfo: %q", got)
	}
	if !strings.Contains(got, "https://"+redactedValue+"@example.com") {
		t.Fatalf("SensitiveText() = %q, want redacted URL userinfo", got)
	}
}

func TestSensitiveTextRedactsSignedURLQueries(t *testing.T) {
	cases := map[string]string{
		"https://acct.blob.core.windows.net/c/b?sv=2024-05-04&se=2026-01-01&sig=abc%2Fdef123&sr=b":                     "https://acct.blob.core.windows.net/c/b?sv=2024-05-04&se=2026-01-01&sig=[REDACTED]&sr=b",
		"https://bucket.s3.amazonaws.com/k?X-Amz-Credential=AKIA%2F20260101&X-Amz-Signature=0123abcd&X-Amz-Expires=60": "https://bucket.s3.amazonaws.com/k?X-Amz-Credential=[REDACTED]", // the generic credential= redactor already consumes the remaining query,
		"see https://storage.googleapis.com/b/o?X-Goog-Signature=deadbeef for details":                                 "see https://storage.googleapis.com/b/o?X-Goog-Signature=[REDACTED] for details",
		"plain https://example.com/path?page=2&sort=asc stays":                                                         "plain https://example.com/path?page=2&sort=asc stays",
		"fetch https://h.example/o?sig=abc123#download, then https://h.example/p?signature=xyz; done":                  "fetch https://h.example/o?sig=[REDACTED]#download, then https://h.example/p?signature=[REDACTED]; done",
	}
	for input, want := range cases {
		if got := SensitiveText(input); got != want {
			t.Fatalf("SensitiveText(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestSensitiveTextRedactsCompleteCommaBearingValue(t *testing.T) {
	got := SensitiveText("dial failed: password=short,correct-horse-battery-staple rejected")
	if strings.Contains(got, "correct-horse-battery-staple") || !strings.Contains(got, "[REDACTED]") {
		t.Fatalf("SensitiveText() = %q, want the complete comma-bearing value redacted", got)
	}
}

func TestSensitiveTextRedactsCookieHeaders(t *testing.T) {
	for _, input := range []string{
		"Cookie: sessionid=correct-horse-battery-staple; theme=dark",
		"Set-Cookie: sessionid=correct-horse-battery-staple; HttpOnly; Secure",
	} {
		got := SensitiveText(input)
		if strings.Contains(got, "correct-horse-battery-staple") || !strings.Contains(got, redactedValue) {
			t.Fatalf("SensitiveText(%q) = %q, want the cookie value redacted", input, got)
		}
	}
}

func TestSensitiveTextRedactsAdjacentQuotedAssignments(t *testing.T) {
	for _, quote := range []string{"'", `"`} {
		for _, second := range []string{"second-placeholder", "second-prefix\\" + quote + "tail-placeholder"} {
			input := "API_KEY=" + quote + "first-placeholder\\" + quote + " PASSWORD=" + quote + second + quote
			got := SensitiveText(input)
			if strings.Contains(got, "first-placeholder") || strings.Contains(got, "second-placeholder") ||
				strings.Contains(got, "second-prefix") || strings.Contains(got, "tail-placeholder") {
				t.Error("adjacent quoted assignment retained credential content")
			}
			if SensitiveText(got) != got {
				t.Error("adjacent assignment redaction was not stable")
			}
		}
	}
}

func TestSensitiveTextRedactsOverlappingCredentialPatterns(t *testing.T) {
	for _, input := range []string{
		`API_KEY='first-placeholder\' token is 'second-placeholder'`,
		`API_KEY="first-placeholder\" token is "second-placeholder"`,
		`API_KEY='first-placeholder\' https://'second-placeholder'@example.com`,
		`API_KEY="first-placeholder\" https://"second-placeholder"@example.com`,
	} {
		got := SensitiveText(input)
		if strings.Contains(got, "first-placeholder") || strings.Contains(got, "second-placeholder") {
			t.Fatal("quoted assignment hid another credential pattern before redaction")
		}
		if SensitiveText(got) != got {
			t.Fatal("overlapping credential redaction was not stable")
		}
	}
}

func TestSensitiveTextWithValuesMatchesOriginalText(t *testing.T) {
	const value = "opaque-assignment-placeholder"
	for _, test := range []struct {
		name, input, want string
		values            []string
	}{
		{"assignment label", "password=" + value, "[REDACTED]=[REDACTED]", []string{"password"}},
		{"quoted label", `{"password":"` + value + `"}`, `{"[REDACTED]":"[REDACTED]"}`, []string{"password"}},
		{"header label", "Authorization: Bearer " + value, "[REDACTED]: [REDACTED]", []string{"Authorization"}},
		{"header value", "Authorization: Bearer part!tail", "Authorization: [REDACTED]", []string{"part!tail"}},
		{"header pattern overlap", "Authorization: Bearer password is " + value, "Authorization: [REDACTED] is [REDACTED]", nil},
		{"signed URL label", "https://example.test?sig=" + value, "https://example.test?[REDACTED]=[REDACTED]", []string{"sig"}},
		{"userinfo label", "https://" + value + "@example.test", "[REDACTED]://[REDACTED]@example.test", []string{"https"}},
		{"overlapping values", "prefix-middle-suffix", "[REDACTED]", []string{"prefix-middle", "middle-suffix"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := SensitiveTextWithValues(test.input, test.values...)
			if got != test.want {
				t.Fatal("redaction lost an original match or changed text outside the matched ranges")
			}
			if SensitiveTextWithValues(got, test.values...) != got {
				t.Fatal("redaction was not stable on a second pass")
			}
		})
	}
}

func TestKnownValuesPreservesUnmatchedPatterns(t *testing.T) {
	const input = "password=untracked-placeholder"
	if got := KnownValues(input, "other-placeholder"); got != input {
		t.Fatal("known-value validation must not redact unrelated patterns in saved history pages")
	}
	for _, test := range []struct {
		input  string
		values []string
	}{
		{"prefix-middle-suffix", []string{"prefix-middle", "middle-suffix"}},
		{"ababab", []string{"abab"}},
		{`opaque\"quoted-placeholder`, []string{`opaque"quoted-placeholder`}},
	} {
		if got := KnownValues(test.input, test.values...); got != redactedValue {
			t.Fatal("known-value redaction retained overlapping or escaped credential content")
		}
	}
}

func TestSensitiveTextPreservesUnambiguousFollowingQuotedAssignments(t *testing.T) {
	for input, want := range map[string]string{
		`API_KEY='fixture\\' note='keep this'`:                     `API_KEY='[REDACTED]' note='keep this'`,
		`API_KEY='fixture\\' note = 'keep this'`:                   `API_KEY='[REDACTED]' note = 'keep this'`,
		`API_KEY="fixture\\" note="keep this"`:                     `API_KEY="[REDACTED]" note="keep this"`,
		`API_KEY='fixture\\' note='keep' PASSWORD='other-fixture'`: `API_KEY='[REDACTED]' note='keep' PASSWORD='[REDACTED]'`,
		`API_KEY='first\' note='`:                                  `API_KEY='[REDACTED]'`,
		`API_KEY='first\' note=\'opaque tail'`:                     `API_KEY='[REDACTED]'`,
		`API_KEY='first\' note="opaque tail" more'`:                `API_KEY='[REDACTED]'`,
	} {
		if got := SensitiveText(input); got != want || SensitiveText(got) != got {
			t.Errorf("SensitiveText(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestSensitiveTextPreservesUnambiguousQuotedFieldsAtCommandBoundaries(t *testing.T) {
	for _, suffix := range []string{"\n", "\r\n", "; echo done", "&& echo done", "| cat"} {
		input := `API_KEY='fixture\\' note='keep this'` + suffix
		want := `API_KEY='[REDACTED]' note='keep this'` + suffix
		if got := SensitiveText(input); got != want || SensitiveText(got) != got {
			t.Errorf("SensitiveText() = %q, want %q", got, want)
		}
	}
}

func TestSensitiveTextPreservesUnambiguousFollowingCommandAssignments(t *testing.T) {
	for _, separator := range []string{";", "; ", " && ", " | "} {
		input := `API_KEY='fixture\\'` + separator + `note='keep this'`
		want := `API_KEY='[REDACTED]'` + separator + `note='keep this'`
		if got := SensitiveText(input); got != want || SensitiveText(got) != got {
			t.Errorf("SensitiveText(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestSensitiveTextPreservesUnambiguousFollowingQuotedCommands(t *testing.T) {
	for _, command := range []string{
		`; printf '%s' 'keep this'`,
		` printf '%s' 'keep this'`,
		` && printf '%s' 'keep this'`,
		`; export NOTE='keep this'`,
	} {
		input := `API_KEY='fixture\\'` + command
		want := `API_KEY='[REDACTED]'` + command
		if got := SensitiveText(input); got != want || SensitiveText(got) != got {
			t.Errorf("SensitiveText(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestSensitiveTextPreservesUnambiguousMixedQuotedCommands(t *testing.T) {
	for _, command := range []string{
		` SAFE="keep one" NOTE='keep two'`,
		` NOTE="it's safe"`,
		` NOTE="$(printf "it's safe")"`,
		` NOTE="$(printf "%s" "it's safe")"`,
		` printf "%s's" 'keep this'`,
		`; SAFE="keep one"; NOTE='keep two'`,
		` printf "%s" 'keep this'`,
		` printf "\"%s\"" 'keep this'`,
		` first="a\"b" second='keep two'`,
		` >"out" printf '%s' 'keep this'`,
		` printf --prefix="good" '%s' 'keep this'`,
	} {
		input := `API_KEY='fixture\\'` + command
		want := `API_KEY='[REDACTED]'` + command
		if got := SensitiveText(input); got != want || SensitiveText(got) != got {
			t.Errorf("SensitiveText(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestSensitiveTextRedactsMixedQuotedAssignments(t *testing.T) {
	input := `API_KEY='first-fixture\\' PASSWORD="second-fixture" NOTE='keep this'`
	want := `API_KEY='[REDACTED]' PASSWORD="[REDACTED]" NOTE='keep this'`
	if got := SensitiveText(input); got != want || SensitiveText(got) != got {
		t.Errorf("SensitiveText(%q) = %q, want %q", input, got, want)
	}
}

func TestSensitiveTextPreservesUnambiguousConcatenatedQuotedWords(t *testing.T) {
	for _, command := range []string{
		`; export NOTE='keep'tail`,
		`; export NOTE='keep'" more"`,
		`; printf '%s'\n 'keep this'`,
		`; printf '%s''more' 'keep this'`,
		`; export NOTE='keep'\''tail'`,
		`; export NOTE=prefix'keep'tail`,
		`; export NOTE="keep"tail OTHER='also'`,
	} {
		input := `API_KEY='fixture\\'` + command
		want := `API_KEY='[REDACTED]'` + command
		if got := SensitiveText(input); got != want || SensitiveText(got) != got {
			t.Errorf("SensitiveText(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestSensitiveTextPreservesUnambiguousFollowingANSIQuotedWords(t *testing.T) {
	for _, command := range []string{
		`; printf $'it\'s-safe'`,
		`; export NOTE=$'it\'s-safe'`,
		`; export NOTE=prefix$'it\'s-safe'tail`,
		`; printf $'it\'s-safe'" and safe"`,
		`; printf $'it\'s-safe\\'`,
	} {
		input := `API_KEY='fixture\\'` + command
		want := `API_KEY='[REDACTED]'` + command
		if got := SensitiveText(input); got != want || SensitiveText(got) != got {
			t.Errorf("SensitiveText(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestSensitiveTextProtectsAmbiguousEscapedCredentialValues(t *testing.T) {
	// Each input is valid shell syntax and a Python assignment whose credential
	// contains the opaque tail. Text alone cannot authorize a shell-only parse.
	for _, input := range []string{
		`API_KEY='fixture\' NOTE="opaque-credential-tail' # safe"`,
		`API_KEY='fixture\' NOTE="$(printf "opaque-credential-tail' # safe")"`,
		`API_KEY='fixture\' # opaque-credential-tail'`,
	} {
		got := SensitiveText(input)
		if strings.Contains(got, "opaque-credential-tail") || strings.Contains(got, "fixture") {
			t.Fatal("ambiguous quoted assignment retained credential content")
		}
		if SensitiveText(got) != got {
			t.Fatal("ambiguous assignment redaction was not stable")
		}
	}
}
