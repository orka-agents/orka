package workspacedelta

import (
	"path"
	"strings"
)

// MatchPathPattern reports whether a repository-relative path matches one
// workspace path policy pattern, the syntax of Task allowedPaths and
// deniedPaths. A pattern ending in /** matches everything below that
// directory; a ** segment matches zero or more whole segments; every other
// segment keeps path.Match single-segment semantics; and a pattern also
// matches the exact path it names. Malformed patterns match nothing, so
// callers that deny on a match must validate patterns first.
func MatchPathPattern(pattern, changedPath string) bool {
	pattern = strings.TrimPrefix(strings.TrimSpace(pattern), "./")
	if pattern == "" {
		return false
	}
	if strings.HasSuffix(pattern, "/**") && strings.HasPrefix(changedPath, strings.TrimSuffix(pattern, "**")) {
		return true
	}
	if pathPatternMatches(pattern, changedPath) {
		return true
	}
	return strings.TrimSuffix(pattern, "/") == strings.TrimSuffix(changedPath, "/")
}

// MatchAnyPathPattern reports whether changedPath matches any pattern. An
// empty pattern list matches nothing.
func MatchAnyPathPattern(patterns []string, changedPath string) bool {
	for _, pattern := range patterns {
		if MatchPathPattern(pattern, changedPath) {
			return true
		}
	}
	return false
}

// PathDenied reports whether a repository-relative path matches any denied
// path pattern, ignoring letter case so a case variant cannot step around a
// protected path. It is shared by every component that enforces Task
// deniedPaths so they agree on what is denied.
func PathDenied(patterns []string, changedPath string) bool {
	if len(patterns) == 0 {
		return false
	}
	if MatchAnyPathPattern(patterns, changedPath) {
		return true
	}
	lowered := strings.ToLower(changedPath)
	for _, pattern := range patterns {
		if MatchPathPattern(strings.ToLower(pattern), lowered) {
			return true
		}
	}
	return false
}

// ValidPathPattern reports whether pattern is a well-formed relative
// workspace path pattern of at most 1024 bytes.
func ValidPathPattern(pattern string) bool {
	cleaned := strings.TrimPrefix(strings.TrimSpace(pattern), "./")
	if cleaned == "" || len(cleaned) > 1024 || strings.ContainsAny(cleaned, "\\\x00") || strings.HasPrefix(cleaned, "/") ||
		cleaned == ".." || strings.HasPrefix(cleaned, "../") || strings.Contains(cleaned, "/../") {
		return false
	}
	_, err := path.Match(cleaned, "validation-probe")
	return err == nil
}

// ValidDeniedPathPattern reports whether pattern is a valid path pattern
// whose segments are all canonical. The matcher compares segments literally,
// so a denied pattern with an empty, "." or ".." segment, such as
// "internal/./evals/**", "docs/" or "internal/evals/..", would never match the
// canonical changed paths it appears to protect; such a pattern is rejected
// instead.
func ValidDeniedPathPattern(pattern string) bool {
	if !ValidPathPattern(pattern) {
		return false
	}
	cleaned := strings.TrimPrefix(strings.TrimSpace(pattern), "./")
	for segment := range strings.SplitSeq(cleaned, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return false
		}
	}
	return true
}

// pathPatternMatches applies gitignore-style glob semantics: a `**` segment
// matches zero or more whole path segments, while every other segment keeps
// path.Match single-segment semantics.
func pathPatternMatches(pattern, changedPath string) bool {
	if !strings.Contains(pattern, "**") {
		matched, err := path.Match(pattern, changedPath)
		return err == nil && matched
	}
	return pathSegmentsMatch(strings.Split(pattern, "/"), strings.Split(changedPath, "/"))
}

// pathSegmentsMatch uses the classic greedy wildcard algorithm at segment
// granularity — backtracking only to the most recent `**` — so matching stays
// O(pattern × path) even for agent-controlled paths against patterns with many
// `**` segments.
func pathSegmentsMatch(patternSegments, pathSegments []string) bool {
	patternIndex, pathIndex := 0, 0
	starPattern, starPath := -1, 0
	for pathIndex < len(pathSegments) {
		switch {
		case patternIndex < len(patternSegments) && patternSegments[patternIndex] == "**":
			starPattern, starPath = patternIndex, pathIndex
			patternIndex++
		case patternIndex < len(patternSegments) && pathSegmentMatches(patternSegments[patternIndex], pathSegments[pathIndex]):
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

func pathSegmentMatches(pattern, segment string) bool {
	matched, err := path.Match(pattern, segment)
	return err == nil && matched
}
