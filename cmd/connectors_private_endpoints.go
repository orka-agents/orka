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

// ConnectorPrivateEndpointsAcknowledgement is the only value that turns on
// --connectors-allow-private-endpoints. Spelling out the consequence is the
// point: a person's linked-account token may be sent to a private address.
const ConnectorPrivateEndpointsAcknowledgement = "i-understand-tokens-may-leave-the-cluster"

// connectorPrivateEndpointsPermitted decides whether the fixture-only
// private-endpoint allowance may be enabled. It requires the literal
// acknowledgement and a plain-http localhost connector callback base URL,
// which only a local or CI deployment can have (production callback bases
// must be public HTTPS origins), so a released configuration cannot turn
// the allowance on by accident.
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

// localhostName accepts the loopback names the connector callback validator
// accepts: localhost and the IPv4/IPv6 loopback literals.
func localhostName(host string) bool {
	switch strings.ToLower(host) {
	case "localhost", "127.0.0.1", "::1":
		return true
	}
	return false
}
