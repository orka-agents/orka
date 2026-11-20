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

// apiAuthorizationDocRow is one route permissions table row.
type apiAuthorizationDocRow struct {
	route                                              string
	group, resource, verb, name, namespace, additional string
}

// apiAuthorizationDocRoutes returns the route permissions table rows: every
// table row whose first two cells are a backticked HTTP method and a
// backticked path.
func apiAuthorizationDocRoutes(doc string) []apiAuthorizationDocRow {
	var routes []apiAuthorizationDocRow
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
		row := apiAuthorizationDocRow{route: method[1] + " " + path[1]}
		if len(cells) >= 8 {
			row.group = strings.TrimSpace(cells[2])
			row.resource = strings.TrimSpace(cells[3])
			row.verb = strings.TrimSpace(cells[4])
			row.name = strings.TrimSpace(cells[5])
			row.namespace = strings.TrimSpace(cells[6])
			row.additional = strings.TrimSpace(strings.Join(cells[7:], "|"))
		}
		routes = append(routes, row)
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
	var duplicates, unknown, mismatched []string
	for _, row := range apiAuthorizationDocRoutes(doc) {
		route := row.route
		if documented[route] {
			duplicates = append(duplicates, route)
		}
		documented[route] = true
		policy, ok := externalAPIPolicies[route]
		if !ok {
			unknown = append(unknown, route)
			continue
		}
		for _, problem := range apiAuthorizationDocRowProblems(row, policy) {
			mismatched = append(mismatched, route+": "+problem)
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
	slices.Sort(mismatched)

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
	if len(mismatched) > 0 {
		problems = append(problems, "rows whose permission columns differ from the policy:\n  "+strings.Join(mismatched, "\n  "))
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

// apiAuthorizationDocNamespaceCodes maps each namespace source to the code the
// route permissions table uses for it.
var apiAuthorizationDocNamespaceCodes = map[apiNamespaceSource]string{
	apiQueryNamespace:         "`Q`",
	apiCreateNamespace:        "`C`",
	apiMetadataNamespace:      "`M`",
	apiBodyNamespace:          "`B`",
	apiQueryThenBodyNamespace: "`QB`",
	apiBodyThenQueryNamespace: "`BQ`",
	apiClusterScoped:          "`cluster`",
	apiUsageTeamNamespaces:    "`Q`",
}

// apiAuthorizationDocRowProblems compares a table row's structured columns
// with its route policy: the first permission fills the group, resource, verb,
// name, and namespace columns, and every further permission must be named in
// the additional-checks column by its verb and resource, so a row may not
// defer its permissions to another row.
func apiAuthorizationDocRowProblems(row apiAuthorizationDocRow, policy apiRoutePolicy) []string {
	var problems []string
	check := func(column, got, want string) {
		if got != want {
			problems = append(problems, fmt.Sprintf("%s is %q, want %q", column, got, want))
		}
	}
	if policy.identityOnly {
		check("API group", row.group, "none")
		check("resource", row.resource, "none")
		check("verb", row.verb, "identity only")
		check("namespace", row.namespace, "not applicable")
		return problems
	}
	if len(policy.permissions) == 0 {
		return []string{"policy has no permissions and is not identity only"}
	}
	primary := policy.permissions[0]
	group := primary.group
	if group == "" {
		group = `""` // the core API group
	}
	check("API group", row.group, "`"+group+"`")
	check("resource", row.resource, "`"+primary.resource+"`")
	check("verb", row.verb, "`"+primary.verb+"`")
	name := "empty"
	if primary.nameParam != "" {
		name = "`:" + primary.nameParam + "`"
		if policy.trimStoreID {
			name += ", trimmed"
		}
	}
	check("name", row.name, name)
	if code, ok := apiAuthorizationDocNamespaceCodes[policy.namespace]; ok {
		check("namespace", row.namespace, code)
	} else {
		problems = append(problems, fmt.Sprintf("namespace source %d has no documented code", policy.namespace))
	}
	for _, extra := range policy.permissions[1:] {
		if !apiAuthorizationDocNamesPermission(row.additional, extra) {
			problems = append(problems, fmt.Sprintf("additional checks do not name `%s` on `%s/%s`", extra.verb, extra.group, extra.resource))
		}
	}
	return problems
}

// apiAuthorizationDocNamesPermission reports whether one semicolon-separated
// clause of an additional-checks cell grants the permission's verb on its
// resource, written "`verb`[, `verb`...] on `[group/]resource`[ and ...]".
// Matching within a clause keeps verbs paired with their own resources and
// SAR names: a named permission's clause must state "`:param`", and an
// unnamed permission's clause must state no URL parameter.
func apiAuthorizationDocNamesPermission(additional string, permission apiResourcePermission) bool {
	for clause := range strings.SplitSeq(additional, ";") {
		verbs, targets, ok := strings.Cut(clause, " on ")
		if !ok || !strings.Contains(verbs, "`"+permission.verb+"`") {
			continue
		}
		if !strings.Contains(targets, "`"+permission.resource+"`") &&
			!strings.Contains(targets, "`"+permission.group+"/"+permission.resource+"`") {
			continue
		}
		// A named permission states its URL parameter; an unnamed one states
		// none (an explicit "empty name" or no name at all).
		if permission.nameParam != "" {
			if strings.Contains(targets, "`:"+permission.nameParam+"`") {
				return true
			}
		} else if !strings.Contains(targets, "`:") {
			return true
		}
	}
	return false
}

func TestAPIAuthorizationDocNamesPermissionPairsVerbsWithResources(t *testing.T) {
	const group = "core.orka.ai"
	create := apiResourcePermission{group, "repositorymonitors/runs", "create", "name"}
	patch := apiResourcePermission{group, "repositorymonitors", "patch", "name"}
	listTasks := apiResourcePermission{group, "tasks", "list", ""}

	correct := "`create` on `core.orka.ai/repositorymonitors/runs`, `:name`; `patch` on `core.orka.ai/repositorymonitors`, `:name`"
	require.True(t, apiAuthorizationDocNamesPermission(correct, create))
	require.True(t, apiAuthorizationDocNamesPermission(correct, patch))

	swapped := "`patch` on `core.orka.ai/repositorymonitors/runs`, `:name`; `create` on `core.orka.ai/repositorymonitors`, `:name`"
	require.False(t, apiAuthorizationDocNamesPermission(swapped, create))
	require.False(t, apiAuthorizationDocNamesPermission(swapped, patch))

	require.True(t, apiAuthorizationDocNamesPermission("`list`, `create`, and `delete` on `core.orka.ai/tasks`, empty name", listTasks))
	require.True(t, apiAuthorizationDocNamesPermission("`list` on `repositorymonitors` and `tasks`", listTasks))
	require.False(t, apiAuthorizationDocNamesPermission("`list` on `repositoryscans/tasks`", listTasks), "a subresource must not match its last segment")
	require.False(t, apiAuthorizationDocNamesPermission("`list` mentioned; `tasks` mentioned", listTasks))

	require.False(t, apiAuthorizationDocNamesPermission("`patch` on `core.orka.ai/repositorymonitors`, empty name", patch), "a named permission must state its parameter")
	require.False(t, apiAuthorizationDocNamesPermission("`patch` on `core.orka.ai/repositorymonitors`, `:id`", patch), "a named permission must state the right parameter")
	require.False(t, apiAuthorizationDocNamesPermission("`list` on `core.orka.ai/tasks`, `:name`", listTasks), "an unnamed permission must not state a parameter")
}
