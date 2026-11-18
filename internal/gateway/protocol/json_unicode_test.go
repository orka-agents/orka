package protocol

import "testing"

func TestValidJSONUnicodePreservesTextWithoutReplacement(t *testing.T) {
	for _, test := range []struct {
		input string
		valid bool
	}{
		{`{"content":"\ud83c\udf0d"}`, true},
		{`{"content":"\\ud800"}`, true},
		{`{"content":"é漢字"}`, true},
		{`{"content":"\ufffd"}`, true},
		{`{"content":"\ud800"}`, false},
		{`{"content":"\udc00"}`, false},
		{`{"content":"\ud800\ud800"}`, false},
		{`{"content":"\ud800x"}`, false},
		{`{"content":"\ud800\u0041"}`, false},
		{"{\"content\":\"\xff\"}", false},
		{`{"content":"ok"}{}`, false},
		{`{"content":"\u12"}`, false},
	} {
		if got := ValidJSONUnicode([]byte(test.input)); got != test.valid {
			t.Errorf("ValidJSONUnicode(%q)=%v, want %v", test.input, got, test.valid)
		}
	}
}
