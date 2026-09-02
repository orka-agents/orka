/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package harness

import "strings"

// WrapperAllowedGitHostsEnv names the optional comma-separated extension to
// the harness v1 wrapper's public repository host allowlist.
const WrapperAllowedGitHostsEnv = "ORKA_HARNESS_WRAPPER_ALLOWED_GIT_HOSTS"

// WrapperGitHostAllowed applies the host policy shared by controller
// pre-binding validation and the harness v1 compatibility wrapper.
func WrapperGitHostAllowed(host, additionalHosts string) bool {
	host = strings.ToLower(strings.TrimSpace(host))
	switch host {
	case "github.com", "gitlab.com", "bitbucket.org":
		return true
	}
	for item := range strings.SplitSeq(additionalHosts, ",") {
		if host != "" && host == strings.ToLower(strings.TrimSpace(item)) {
			return true
		}
	}
	return false
}
