package intake

import (
	"reflect"
	"strings"
	"testing"
)

func TestRejectInvalidJSONWithoutEcho(t *testing.T) {
	t.Parallel()
	for name, source := range map[string][]byte{
		"empty":              {},
		"array":              []byte(`[]`),
		"null":               []byte(`null`),
		"scalar":             []byte(`"PRIVATE_ADMIN_VALUE"`),
		"malformed":          []byte(`{"customer":"PRIVATE_ADMIN_VALUE",`),
		"duplicate":          []byte(`{"title":"Synthetic","title":"PRIVATE_ADMIN_VALUE"}`),
		"escaped duplicate":  []byte(`{"title":"Synthetic","ti\u0074le":"PRIVATE_ADMIN_VALUE"}`),
		"case collision":     []byte(`{"restricted":false,"Restricted":true}`),
		"nested duplicate":   []byte(`{"customer":{"PRIVATE_ADMIN_KEY":"PRIVATE_ADMIN_VALUE","PRIVATE_ADMIN_KEY":0}}`),
		"trailing object":    []byte(`{"problem":"Synthetic"} {"tenant":"PRIVATE_ADMIN_VALUE"}`),
		"trailing junk":      []byte(`{"problem":"Synthetic"} PRIVATE_ADMIN_VALUE`),
		"invalid UTF8":       append([]byte(`{"problem":"`), 0xff, '"', '}'),
		"lone surrogate":     []byte(`{"problem":"\ud800"}`),
		"low surrogate":      []byte(`{"problem":"\udc00"}`),
		"mismatched pair":    []byte(`{"problem":"\ud800\u0041"}`),
		"escaped second":     []byte(`{"problem":"\ud800\\udc00"}`),
		"invalid hex escape": []byte(`{"problem":"\uZZZZ"}`),
		"truncated escape":   []byte(`{"problem":"\u00`),
	} {
		t.Run(name, func(t *testing.T) {
			report, err := Parse(source)
			if err == nil {
				t.Fatal("invalid source accepted")
			}
			if strings.Contains(err.Error(), "PRIVATE_ADMIN_") {
				t.Fatal("JSON error echoed an administrative field or value")
			}
			if !reflect.DeepEqual(report, Report{}) {
				t.Fatal("invalid JSON returned a partial normalized report")
			}
		})
	}
}

func TestValidUnicode(t *testing.T) {
	t.Parallel()
	for _, source := range []string{
		`{"problem":"Synthetic \ud83d\ude00"}`,
		`{"problem":"Synthetic \\ud800 literal"}`,
		`{"problem":"Synthetic \ufffd replacement character"}`,
		`{"problem":"Synthetic quote: \" and slash: \\"}`,
	} {
		parseFixture(t, []byte(source))
	}
}

func TestJSONDepthAndNodeLimits(t *testing.T) {
	t.Parallel()
	nested := func(count int) []byte {
		return []byte(`{"problem":"Synthetic","administration":` + strings.Repeat("[", count) + `0` + strings.Repeat("]", count) + `}`)
	}
	parseFixture(t, nested(maxJSONDepth-1))
	if _, err := Parse(nested(maxJSONDepth)); err == nil {
		t.Fatal("deep administrative content bypassed the nesting safeguard")
	}
	exact := []byte(`{"problem":"Synthetic","administration":[` + strings.Repeat("0,", maxJSONValues-6) + `0]}`)
	parseFixture(t, exact)
	source := []byte(`{"problem":"Synthetic","administration":[` + strings.Repeat("0,", maxJSONValues) + `0]}`)
	if _, err := Parse(source); err == nil {
		t.Fatal("administrative arrays bypassed the value count safeguard")
	}
}

func TestJSONPointerEscaping(t *testing.T) {
	t.Parallel()
	if pointer("/context", "synthetic~/field") != "/context/synthetic~0~1field" {
		t.Fatal("pointer tokens were not RFC 6901 escaped")
	}
}

func FuzzParse(f *testing.F) {
	for _, seed := range []string{
		`{"title":"Synthetic","problem":"Synthetic body"}`,
		`{"summary":"<p>Synthetic body</p>","restricted":true}`,
		`{"problem":"Synthetic","administration":{"nested":[1,2,3]}}`,
		`{"problem":"Synthetic \ud800"}`,
	} {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		report, err := Parse(data)
		if err != nil {
			if !reflect.DeepEqual(report, Report{}) {
				t.Fatal("error returned a partial report")
			}
			return
		}
		if report.Version != SchemaVersion || report.Sections == nil ||
			len(report.Sections) > maxSections || len(report.Repositories) > maxRepositories ||
			len(report.Versions) > maxVersions || len(marshalFixture(t, report)) > maxDerivedBytes {
			t.Fatal("successful parse violated the normalized schema")
		}
		assertPointersResolve(t, data, report)
		for _, repository := range report.Repositories {
			if !canonicalRepository(repository) {
				t.Fatal("successful parse returned an unsafe repository candidate")
			}
		}
	})
}
