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

type NoEffectError struct {
	Summary string
	Err     error
}

func (e *NoEffectError) Error() string { return e.Err.Error() }
func (e *NoEffectError) Unwrap() error { return e.Err }

func notSent(err error) error {
	if err == nil {
		return nil
	}
	return &NoEffectError{Summary: "The request was not sent: " + err.Error(), Err: err}
}

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

func followedRedirect(resp *http.Response) bool {
	return resp.Request != nil && resp.Request.Response != nil
}
