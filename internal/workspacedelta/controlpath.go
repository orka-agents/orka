package workspacedelta

import "strings"

// RepositoryControlPath reports whether a repository-relative path is a
// repository control path that a workspace denying control paths may not
// change: CI workflows, RBAC manifests, and chart Secret templates.
func RepositoryControlPath(repositoryRelativePath string) bool {
	lower := strings.ToLower(strings.TrimPrefix(repositoryRelativePath, "./"))
	return strings.HasPrefix(lower, ".github/workflows/") ||
		strings.HasPrefix(lower, "config/rbac/") ||
		(strings.HasPrefix(lower, "charts/") && strings.Contains(lower, "secret"))
}
