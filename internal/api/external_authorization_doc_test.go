package api

import (
	"fmt"
	"net/http"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// apiAuthorizationDocPath is the operator reference that documents every
// external route policy.
const apiAuthorizationDocPath = "../../website/docs/reference/api-authorization.md"

var (
	apiAuthorizationDocRoutePattern = regexp.MustCompile("^`([A-Z]+)`$")
	apiAuthorizationDocPathPattern  = regexp.MustCompile("^`(/[^`]*)`$")
	apiAuthorizationDocCountPattern = regexp.MustCompile(
		`covers all (\d+) authenticated external route registrations, including (\d+) non-GET registrations under ` + "`/api/v1`")
)

// apiAuthorizationDocRoutes returns the "METHOD /path" keys listed in the
// route permissions table: every table row whose first two cells are a
// backticked HTTP method and a backticked path.
func apiAuthorizationDocRoutes(doc string) []string {
	var routes []string
	for line := range strings.SplitSeq(doc, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "|") {
			continue
		}
		cells := strings.Split(strings.Trim(line, "|"), "|")
		if len(cells) < 2 {
			continue
		}
		method := apiAuthorizationDocRoutePattern.FindStringSubmatch(strings.TrimSpace(cells[0]))
		path := apiAuthorizationDocPathPattern.FindStringSubmatch(strings.TrimSpace(cells[1]))
		if method == nil || path == nil {
			continue
		}
		routes = append(routes, method[1]+" "+path[1])
	}
	return routes
}

// TestExternalAPIRouteInventoryMatchesAuthorizationDoc keeps the operator
// reference in sync with externalAPIPolicies. TestExternalAPIRouteInventory
// already ties every registered route to a policy and an HTTP test.
func TestExternalAPIRouteInventoryMatchesAuthorizationDoc(t *testing.T) {
	raw, err := os.ReadFile(apiAuthorizationDocPath)
	require.NoError(t, err)
	doc := string(raw)

	documented := map[string]bool{}
	var duplicates, unknown []string
	for _, route := range apiAuthorizationDocRoutes(doc) {
		if documented[route] {
			duplicates = append(duplicates, route)
		}
		documented[route] = true
		if _, ok := externalAPIPolicies[route]; !ok {
			unknown = append(unknown, route)
		}
	}
	var missing []string
	nonGetV1 := 0
	for route := range externalAPIPolicies {
		if !documented[route] {
			missing = append(missing, route)
		}
		if !strings.HasPrefix(route, http.MethodGet+" ") && strings.Contains(route, " /api/v1/") {
			nonGetV1++
		}
	}
	slices.Sort(missing)
	slices.Sort(unknown)
	slices.Sort(duplicates)

	var problems []string
	if len(missing) > 0 {
		problems = append(problems, "routes with a policy but no row:\n  "+strings.Join(missing, "\n  "))
	}
	if len(unknown) > 0 {
		problems = append(problems, "rows with no policy in externalAPIPolicies:\n  "+strings.Join(unknown, "\n  "))
	}
	if len(duplicates) > 0 {
		problems = append(problems, "duplicate rows:\n  "+strings.Join(duplicates, "\n  "))
	}

	// The introduction states the inventory size; keep it accurate too.
	counts := apiAuthorizationDocCountPattern.FindStringSubmatch(strings.Join(strings.Fields(doc), " "))
	if counts == nil {
		problems = append(problems, "introduction no longer states \"covers all N authenticated external route registrations, "+
			"including M non-GET registrations under `/api/v1`\"; restore it or update this test")
	} else {
		if total, _ := strconv.Atoi(counts[1]); total != len(externalAPIPolicies) {
			problems = append(problems, fmt.Sprintf("introduction says %d route registrations; externalAPIPolicies has %d", total, len(externalAPIPolicies)))
		}
		if nonGet, _ := strconv.Atoi(counts[2]); nonGet != nonGetV1 {
			problems = append(problems, fmt.Sprintf("introduction says %d non-GET /api/v1 registrations; externalAPIPolicies has %d", nonGet, nonGetV1))
		}
	}

	if len(problems) > 0 {
		t.Fatalf("%s is out of sync with externalAPIPolicies (internal/api/external_authorization.go). "+
			"Update the doc's route permissions table so each policy has exactly one row with its permissions:\n%s",
			strings.TrimPrefix(apiAuthorizationDocPath, "../../"), strings.Join(problems, "\n"))
	}
}
