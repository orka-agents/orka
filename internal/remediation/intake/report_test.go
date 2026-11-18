package intake

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
)

func TestParseSuppliedReports(t *testing.T) {
	t.Parallel()
	for _, repository := range []string{
		"https://github.com/fixture-org/go-widget", "https://github.com/fixture-org/c-parser",
		"https://github.com/Another-Fixture/http.service", "https://github.com/fixture-team/config_rules",
	} {
		data := marshalFixture(t, map[string]any{
			"title": "Synthetic boundary handling", "problem": "An empty input incorrectly succeeds.",
			"description": "Reject empty input before parsing.", "repository": repository,
			"versions": []string{"v1.2.3", "2.0.0-rc.1"}, "restricted": false,
			"customer": "OMITTED_CUSTOMER", "contact": "OMITTED_CONTACT",
		})
		report := parseFixture(t, data)
		if report.Version != SchemaVersion || report.SourceKind != "supplied-report" ||
			report.Title != "Synthetic boundary handling" || report.Restricted ||
			!slices.Equal(report.Repositories, []string{repository}) ||
			!slices.Equal(report.Versions, []string{"v1.2.3", "2.0.0-rc.1"}) {
			t.Fatal("supplied technical report changed")
		}
		assertNoMarker(t, report, "OMITTED_")
		assertWarning(t, report, "reported_versions_are_unverified")
		assertWarning(t, report, "normalization_does_not_authorize_disclosure")
		assertPointersResolve(t, data, report)
	}
}

func TestRestrictionDefaults(t *testing.T) {
	for _, test := range []struct {
		raw  string
		want bool
	}{
		{`{"problem":"Synthetic"}`, true},
		{`{"problem":"Synthetic","restricted":true}`, true},
		{`{"problem":"Synthetic","restricted":false}`, false},
	} {
		if got := parseFixture(t, []byte(test.raw)); got.Restricted != test.want {
			t.Fatal("restriction was not preserved")
		}
	}
}

func TestMissingTechnicalDetailsIsExplicit(t *testing.T) {
	for _, raw := range []string{`{}`, `{"title":"Synthetic"}`, `{"summary":"<script>hidden</script>"}`, `{"metadata":{"description":"omitted"}}`} {
		report := parseFixture(t, []byte(raw))
		assertWarning(t, report, "missing_technical_details")
		if report.Sections == nil {
			t.Fatal("sections must encode as an array")
		}
	}
}

func TestExactInputDigestAndOmittedFields(t *testing.T) {
	raw := []byte(`{"problem":"Synthetic","metadata":"omitted-a"}`)
	first := parseFixture(t, raw)
	sum := sha256.Sum256(raw)
	if first.SourceDigest != "sha256:"+hex.EncodeToString(sum[:]) {
		t.Fatal("digest does not cover exact input bytes")
	}
	spaced := parseFixture(t, append(bytes.Clone(raw), ' '))
	if spaced.SourceDigest == first.SourceDigest {
		t.Fatal("changed bytes retained the same input identity")
	}
	second := parseFixture(t, []byte(strings.Replace(string(raw), "omitted-a", "omitted-b", 1)))
	if first.SourceDigest == second.SourceDigest {
		t.Fatal("omitted fields must still affect source identity")
	}
	first.SourceDigest, second.SourceDigest = "", ""
	if !reflect.DeepEqual(first, second) {
		t.Fatal("administrative content affected selected technical data")
	}
}

func TestInvalidSelectedShapesDoNotEchoInput(t *testing.T) {
	for _, raw := range []string{
		`{"title":{"private":"PRIVATE_MARKER"}}`, `{"problem":["PRIVATE_MARKER"]}`,
		`{"restricted":"PRIVATE_MARKER"}`, `{"restricted":null}`,
		`{"repository":{}}`, `{"versions":[{}]}`, `{"versions":["PRIVATE_MARKER"]}`,
	} {
		report, err := Parse([]byte(raw))
		if err == nil || strings.Contains(err.Error(), "PRIVATE_MARKER") || !reflect.DeepEqual(report, Report{}) {
			t.Fatal("invalid report was accepted or exposed partial input")
		}
	}
}

func TestReportLimits(t *testing.T) {
	for _, field := range []struct {
		name  string
		limit int
	}{{"problem", maxSectionBytes}, {"title", maxTitleBytes}} {
		for _, delta := range []int{-1, 0, 1} {
			raw := marshalFixture(t, map[string]any{field.name: strings.Repeat("x", field.limit+delta)})
			report, err := Parse(raw)
			if delta > 0 {
				if err == nil || !reflect.DeepEqual(report, Report{}) {
					t.Fatal("over-limit input returned success or partial state")
				}
			} else if err != nil || len(report.Sections[0].Text) != field.limit+delta {
				t.Fatal("bounded input was rejected or truncated")
			}
		}
	}
	prefix, suffix := `{"problem":"Synthetic","metadata":"`, `"}`
	exact := []byte(prefix + strings.Repeat("x", maxInputBytes-len(prefix)-len(suffix)) + suffix)
	parseFixture(t, exact)
	if _, err := Parse(append(exact, ' ')); err == nil {
		t.Fatal("input larger than 8 MiB was accepted")
	}
	for _, delta := range []int{-1, 0, 1} {
		base := parseFixture(t, []byte(`{"problem":"x"}`))
		length := maxDerivedBytes + delta - len(marshalFixture(t, base)) + 1
		body := strings.Repeat("&", length/6) + strings.Repeat("x", length%6)
		report, err := Parse(marshalFixture(t, map[string]any{"problem": body}))
		if delta > 0 {
			if err == nil || !reflect.DeepEqual(report, Report{}) {
				t.Fatal("encoded JSON limit was not enforced")
			}
		} else if err != nil || len(marshalFixture(t, report)) != maxDerivedBytes+delta {
			t.Fatal("encoded JSON boundary was rejected or measured incorrectly")
		}
	}
	for _, limit := range []int{maxVersions - 1, maxVersions, maxVersions + 1} {
		versions := make([]string, limit)
		for i := range versions {
			versions[i] = "1.2." + strconv.Itoa(i)
		}
		report, err := Parse(marshalFixture(t, map[string]any{"versions": versions}))
		if limit > maxVersions {
			if err == nil {
				t.Fatal("too many versions accepted")
			}
		} else if err != nil || len(report.Versions) != limit {
			t.Fatal("bounded versions were dropped")
		}
	}
}

func marshalFixture(t *testing.T, value any) []byte {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal("could not marshal synthetic fixture")
	}
	return raw
}

func parseFixture(t *testing.T, raw []byte) Report {
	t.Helper()
	report, err := Parse(raw)
	if err != nil {
		t.Fatalf("synthetic fixture failed: %v", err)
	}
	return report
}

func assertWarning(t *testing.T, report Report, warning string) {
	t.Helper()
	if !slices.Contains(report.Warnings, warning) {
		t.Fatalf("expected fixed warning %q", warning)
	}
}

func assertNoMarker(t *testing.T, report Report, marker string) {
	t.Helper()
	if strings.Contains(string(marshalFixture(t, report)), marker) {
		t.Fatal("normalized report leaked an omitted synthetic marker")
	}
}

func assertPointersResolve(t *testing.T, raw []byte, report Report) {
	t.Helper()
	var root any
	if json.Unmarshal(raw, &root) != nil {
		t.Fatal("could not decode synthetic fixture")
	}
	for _, section := range report.Sections {
		value := root
		if !strings.HasPrefix(section.SourcePointer, "/") {
			t.Fatal("missing source pointer")
		}
		for token := range strings.SplitSeq(section.SourcePointer[1:], "/") {
			token = strings.ReplaceAll(strings.ReplaceAll(token, "~1", "/"), "~0", "~")
			switch object := value.(type) {
			case map[string]any:
				var exists bool
				value, exists = object[token]
				if !exists {
					t.Fatal("source pointer does not resolve")
				}
			case []any:
				index, err := strconv.Atoi(token)
				if err != nil || index < 0 || index >= len(object) {
					t.Fatal("invalid array pointer")
				}
				value = object[index]
			default:
				t.Fatal("pointer crosses a scalar")
			}
		}
		if _, ok := value.(string); !ok {
			t.Fatal("section pointer does not identify original text")
		}
	}
}
