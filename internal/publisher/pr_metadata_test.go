package publisher

import (
	"errors"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestDefaultPullRequestTitle(t *testing.T) {
	for _, test := range []struct {
		name, prompt, want string
	}{
		{name: "first line", prompt: "fix: reject empty filters\nImplementation details", want: "fix: reject empty filters"},
		{name: "blank lines and CRLF", prompt: " \r\n\t\n  fix: keep whitespace inside  title \r\nMore", want: "fix: keep whitespace inside  title"},
		{name: "empty", want: "Orka publication generation 7"},
		{name: "whitespace only", prompt: " \n\t\r", want: "Orka publication generation 7"},
		{name: "Unicode whitespace only", prompt: "\u0085\u00a0\u2003\u3000", want: "Orka publication generation 7"},
		{name: "bounded ASCII", prompt: strings.Repeat("x", 257), want: strings.Repeat("x", 256)},
		{name: "bounded Unicode", prompt: strings.Repeat("界🙂", 129), want: strings.Repeat("界🙂", 128)},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := DefaultPullRequestTitle(test.prompt, 7)
			if got != test.want || !utf8.ValidString(got) || utf8.RuneCountInString(got) > MaxPullRequestTitleLength {
				t.Fatalf("DefaultPullRequestTitle() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestValidatePullRequestText(t *testing.T) {
	for _, char := range []string{"a", "界", "🙂"} {
		if err := ValidatePullRequestText(strings.Repeat(char, MaxPullRequestTitleLength), strings.Repeat(char, MaxPullRequestBodyLength)); err != nil {
			t.Fatalf("valid character bounds: %v", err)
		}
		if err := ValidatePullRequestText(strings.Repeat(char, MaxPullRequestTitleLength+1), ""); err == nil {
			t.Fatal("accepted overlong title")
		}
		if err := ValidatePullRequestText("", strings.Repeat(char, MaxPullRequestBodyLength+1)); err == nil {
			t.Fatal("accepted overlong body")
		}
	}
}

func TestValidatePullRequestTextAcceptsBenignText(t *testing.T) {
	for _, test := range []struct {
		name, title, body string
	}{
		{name: "empty fallback"},
		{name: "explicit title with whitespace", title: " \tfix: preserve exact \u2003 title\u00a0", body: "## Summary\n\nPreserve the title.\n"},
		{name: "whitespace body", title: "fix: preserve body", body: " \t\n\u0085\u00a0\u2003\u3000"},
		{name: "credential placeholders", title: "docs: explain API keys", body: "Use OPENAI_API_KEY=dummy and Authorization: Bearer $TOKEN."},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := ValidatePullRequestText(test.title, test.body); err != nil {
				t.Fatalf("rejected benign pull request text: %v", err)
			}
		})
	}
}

func TestValidatePullRequestTextRejectsWhitespaceTitle(t *testing.T) {
	for _, test := range []struct {
		name, title string
	}{
		{name: "spaces", title: "   "},
		{name: "tabs", title: "\t\t"},
		{name: "newlines", title: "\n\r\n"},
		{name: "Unicode whitespace", title: "\u0085\u00a0\u1680\u2000\u2001\u2002\u2003\u2004\u2005\u2006\u2007\u2008\u2009\u200a\u2028\u2029\u202f\u205f\u3000"},
		{name: "mixed whitespace", title: " \t\n\r\v\f\u00a0\u2003\u3000"},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := ValidatePullRequestText(test.title, "A regular body.")
			if !errors.Is(err, ErrInvalidRequest) || err.Error() != "validate prTitle: must not be whitespace-only" {
				t.Fatal("whitespace-only title did not return the expected validation error")
			}
		})
	}
}

func TestValidatePullRequestTextRejectsSecretLikeText(t *testing.T) {
	// Build unmistakably fake values without storing or logging token-shaped literals.
	fakeToken := "g" + "hp_" + strings.Repeat("NOTAREALSECRET", 3)
	fakeAssignment := "password=" + strings.Repeat("FAKE_TEST_ONLY_", 3)
	for _, test := range []struct {
		name, title, body, field string
	}{
		{name: "token in title", title: "fix: remove " + fakeToken, field: "prTitle"},
		{name: "assignment in title", title: fakeAssignment, field: "prTitle"},
		{name: "token in body", title: "fix: remove credential", body: "## Details\n\n" + fakeToken, field: "prBody"},
		{name: "assignment in body", title: "fix: remove credential", body: "## Details\n\n" + fakeAssignment, field: "prBody"},
		{name: "token in prompt-derived title", title: DefaultPullRequestTitle(" \nfix: remove "+fakeToken+"\nMore details", 7), field: "prTitle"},
		{name: "assignment in prompt-derived title", title: DefaultPullRequestTitle(fakeAssignment+"\nMore details", 7), field: "prTitle"},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := ValidatePullRequestText(test.title, test.body)
			if !errors.Is(err, ErrInvalidRequest) || err.Error() != "validate "+test.field+": must not contain credentials or tokens" {
				// Do not print the error: a regression could include the fake value.
				t.Fatal("secret-like text did not return the expected non-sensitive validation error")
			}
		})
	}
}
