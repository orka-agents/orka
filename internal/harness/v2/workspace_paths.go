package v2

import (
	"path"
	"strings"
)

// WorkspaceDeltaPathAllowed reports whether changedPath matches one of the
// allowed path patterns. An empty pattern list allows every path. A pattern
// ending in "/**" allows everything under its directory, a pattern equal to the
// path allows it, and any other pattern is a glob with gitignore-style "**"
// segments.
func WorkspaceDeltaPathAllowed(changedPath string, patterns []string) bool {
	if len(patterns) == 0 {
		return true
	}
	for _, patternValue := range patterns {
		patternValue = strings.TrimPrefix(strings.TrimSpace(patternValue), "./")
		if strings.HasSuffix(patternValue, "/**") && strings.HasPrefix(changedPath, strings.TrimSuffix(patternValue, "**")) {
			return true
		}
		if workspaceDeltaPatternMatches(patternValue, changedPath) {
			return true
		}
		if strings.TrimSuffix(patternValue, "/") == strings.TrimSuffix(changedPath, "/") {
			return true
		}
	}
	return false
}

// WorkspaceDeltaRepositoryControlPath reports whether changedPath is a
// repository-control path that write limits always protect: CI workflows,
// RBAC configuration, and chart files that name secrets.
func WorkspaceDeltaRepositoryControlPath(changedPath string) bool {
	lower := strings.ToLower(strings.TrimPrefix(changedPath, "./"))
	return strings.HasPrefix(lower, ".github/workflows/") ||
		strings.HasPrefix(lower, "config/rbac/") ||
		(strings.HasPrefix(lower, "charts/") && strings.Contains(lower, "secret"))
}

// workspaceDeltaPatternMatches applies gitignore-style glob semantics: a `**`
// segment matches zero or more whole path segments, while every other segment
// keeps path.Match single-segment semantics.
func workspaceDeltaPatternMatches(patternValue, changedPath string) bool {
	if !strings.Contains(patternValue, "**") {
		matched, err := path.Match(patternValue, changedPath)
		return err == nil && matched
	}
	return workspaceDeltaSegmentsMatch(strings.Split(patternValue, "/"), strings.Split(changedPath, "/"))
}

// workspaceDeltaSegmentsMatch uses the classic greedy wildcard algorithm at
// segment granularity — backtracking only to the most recent `**` — so
// matching stays O(pattern × path) even for agent-controlled paths against
// patterns with many `**` segments.
func workspaceDeltaSegmentsMatch(patternSegments, pathSegments []string) bool {
	patternIndex, pathIndex := 0, 0
	starPattern, starPath := -1, 0
	for pathIndex < len(pathSegments) {
		switch {
		case patternIndex < len(patternSegments) && patternSegments[patternIndex] == "**":
			starPattern, starPath = patternIndex, pathIndex
			patternIndex++
		case patternIndex < len(patternSegments) && workspaceDeltaSegmentMatches(patternSegments[patternIndex], pathSegments[pathIndex]):
			patternIndex++
			pathIndex++
		case starPattern >= 0:
			starPath++
			pathIndex = starPath
			patternIndex = starPattern + 1
		default:
			return false
		}
	}
	for patternIndex < len(patternSegments) && patternSegments[patternIndex] == "**" {
		patternIndex++
	}
	return patternIndex == len(patternSegments)
}

func workspaceDeltaSegmentMatches(pattern, segment string) bool {
	matched, err := path.Match(pattern, segment)
	return err == nil && matched
}
