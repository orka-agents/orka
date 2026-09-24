package publisher

import (
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
