/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package main

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
)

const ConnectorPrivateEndpointsAcknowledgement = "i-understand-tokens-may-leave-the-cluster"

func connectorPrivateEndpointsPermitted(acknowledgement, callbackBaseURL string) (bool, error) {
	acknowledgement = strings.TrimSpace(acknowledgement)
	if acknowledgement == "" {
		return false, nil
	}
	if acknowledgement != ConnectorPrivateEndpointsAcknowledgement {
		return false, fmt.Errorf("--connectors-allow-private-endpoints accepts only the literal %q",
			ConnectorPrivateEndpointsAcknowledgement)
	}
	parsed, err := url.Parse(strings.TrimSpace(callbackBaseURL))
	if err != nil || parsed.Scheme != "http" || !localhostName(parsed.Hostname()) {
		return false, errors.New("--connectors-allow-private-endpoints is for local fixtures only: " +
			"it requires a plain-http localhost --connector-callback-base-url")
	}
	return true, nil
}

func localhostName(host string) bool {
	switch strings.ToLower(host) {
	case "localhost", "127.0.0.1", "::1":
		return true
	}
	return false
}
