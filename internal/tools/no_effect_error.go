/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package tools

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
)

// NoEffectError reports a tool call that definitely had no external effect:
// it stopped before sending its request, or the remote service refused the
// request outright. A caller that records uncertain outcomes may settle it
// as a definite failure instead of one that may have run. Summary is text for
// a reviewer and the model; it can quote the remote service's message, so a
// caller redacts it before bounding it. Error keeps the full cause for logs.
type NoEffectError struct {
	Summary string
	Err     error
}

func (e *NoEffectError) Error() string { return e.Err.Error() }
func (e *NoEffectError) Unwrap() error { return e.Err }

// notSent marks err, returned before a tool sent its request, as having no
// effect.
func notSent(err error) error {
	if err == nil {
		return nil
	}
	return &NoEffectError{Summary: "The request was not sent: " + err.Error(), Err: err}
}

// githubStatusError is the error for a non-2xx GitHub API response to a
// mutation. A 4xx other than 408 Request Timeout refuses the request, so it
// had no effect, unless it answered a followed redirect: the original request
// may have taken effect before redirecting. Any other status leaves the
// outcome uncertain.
func githubStatusError(resp *http.Response, body []byte) error {
	status := resp.StatusCode
	err := fmt.Errorf("GitHub API returned %d: %s", status, string(body))
	if status < 400 || status >= 500 || status == http.StatusRequestTimeout || followedRedirect(resp) {
		return err
	}
	summary := fmt.Sprintf("GitHub refused the request (HTTP %d)", status)
	var apiErr gitHubAPIErrorResponse
	if json.Unmarshal(body, &apiErr) == nil {
		details := []string{apiErr.Message}
		for _, item := range apiErr.Errors {
			details = append(details, item.Message)
		}
		details = slices.DeleteFunc(details, func(message string) bool { return strings.TrimSpace(message) == "" })
		if len(details) > 0 {
			summary += ": " + strings.Join(details, "; ")
		}
	}
	return &NoEffectError{Summary: summary, Err: err}
}

// followedRedirect reports whether resp answered a request created by a
// followed redirect, which carries the response that caused it.
func followedRedirect(resp *http.Response) bool {
	return resp.Request != nil && resp.Request.Response != nil
}
